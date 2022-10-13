// Command qutest provides a testing CLI for QUnit based tests. The tests are
// run using ChromeDP.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	cdruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/davecgh/go-spew/spew"
	esbapi "github.com/evanw/esbuild/pkg/api"
	esbcli "github.com/evanw/esbuild/pkg/cli"
	"github.com/jpillora/opts"
	"github.com/kgadams/go-shellquote"
	"github.com/pkg/errors"
)

var Dump = spew.Dump

//go:generate mkdir -p assets
//go:generate curl -o assets/qunit.css https://code.jquery.com/qunit/qunit-2.19.1.css
//go:generate curl -o assets/qunit.js https://code.jquery.com/qunit/qunit-2.19.1.js

//go:embed assets/qunit.css
var qunitCSS []byte

//go:embed assets/qunit.js
var qunitJS []byte

var defaultInclude = []string{"**/*.js", "**/*.ts", "**/*.jsx", "**/*.tsx"}

const enableTimeit = false

func timeit(label string) func() {
	if !enableTimeit {
		return func() {}
	}
	start := time.Now()
	return func() {
		println(label, "took", msSince(start).String())
	}
}

func msSince(start time.Time) time.Duration {
	return time.Since(start).Truncate(time.Millisecond)
}

type args struct {
	Root        string        `opts:"help=root directory"`
	Include     []string      `opts:"mode=arg,help=globs to include"`
	Exclude     []string      `opts:"help=globs to exclude"`
	ESBuild     string        `opts:"name=esbuild,help=esbuild arguments (as single string argument)"`
	Coverage    bool          `opts:"help=enable code coverage"`
	Timeout     time.Duration `opts:"help=timeout for all tests"`
	Parallel    int           `opts:"help=number of parallel tests"`
	Watch       bool          `opts:"help=watch mode"`
	Visible     bool          `opts:"help=run visible browser"`
	KeepRunning bool          `opts:"help=keep browser running after tests"`
	Port        int           `opts:"help=use specific port for internal server"`
}

func testServer(ctx context.Context, args *args) (*http.Server, error) {
	esbuildArgs, err := shellquote.Split(args.ESBuild)
	if err != nil {
		return nil, errors.WithMessage(err, "invalid format for esbuild arguments")
	}
	buildOptions, err := esbcli.ParseBuildOptions(esbuildArgs)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	buildOptions.Bundle = true
	buildOptions.Sourcemap = esbapi.SourceMapInline
	buildOptions.Outbase = ""
	buildOptions.Outdir = "dist"
	buildOptions.Format = esbapi.FormatESModule

	mux := http.NewServeMux()
	mux.HandleFunc("/test/", func(w http.ResponseWriter, r *http.Request) {
		src := strings.TrimPrefix(r.URL.Path, "/test")
		err := indexHTML.Execute(w,
			struct {
				TestSrc string
			}{
				TestSrc: path.Join("/bundle", src),
			})
		if err != nil {
			log.Println(err)
		}
	})
	mux.HandleFunc("/bundle/", func(w http.ResponseWriter, r *http.Request) {
		src := strings.TrimPrefix(r.URL.Path, "/bundle/")
		opts := buildOptions
		opts.EntryPoints = []string{src}
		result := esbapi.Build(opts)
		if len(result.Errors) > 0 {
			w.WriteHeader(500)
			e := json.NewEncoder(w)
			e.SetIndent("", "  ")
			e.Encode(result.Errors)
			return
		}
		w.Header().Set("content-type", mime.TypeByExtension(".js"))
		w.Write(result.OutputFiles[0].Contents)
	})
	mux.HandleFunc("/qunit.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", mime.TypeByExtension(".js"))
		w.Write(qunitJS)
	})
	mux.HandleFunc("/qunit.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", mime.TypeByExtension(".css"))
		w.Write(qunitCSS)
	})
	addr := "127.0.0.1:0"
	if args.Port != 0 {
		addr = fmt.Sprintf("127.0.0.1:%d", args.Port)
	}
	l, err := net.Listen("tcp4", addr)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	server := &http.Server{
		Addr:    l.Addr().String(), // to allow finding out assigned port
		Handler: mux,
		BaseContext: func(l net.Listener) context.Context {
			return ctx
		},
	}
	go func() {
		if err := server.Serve(l); err != nil {
			log.Fatal(err)
		}
	}()
	return server, nil
}

func longestCommonPrefix(vs []string) string {
	longestPrefix := ""
	endPrefix := false

	if len(vs) > 0 {
		sort.Strings(vs)
		first := vs[0]
		last := vs[len(vs)-1]

		for i := 0; i < len(first); i++ {
			if !endPrefix && last[i] == first[i] {
				longestPrefix += string(last[i])
			} else {
				endPrefix = true
			}
		}
	}
	return longestPrefix
}

func HeadlessFalse(a *chromedp.ExecAllocator) {
	chromedp.Flag("headless", false)(a)
}

func ExposeFunc(name string, f func(string)) chromedp.Action {
	return chromedp.Tasks{
		chromedp.ActionFunc(func(ctx context.Context) error {
			chromedp.ListenTarget(ctx, func(ev interface{}) {
				if ev, ok := ev.(*cdruntime.EventBindingCalled); ok && ev.Name == name {
					f(ev.Payload)
				}
			})
			return nil
		}),
		cdruntime.AddBinding(name),
	}
}

type qunitRunEnd struct {
	FullName   []string `json:"fullName"`
	Runtime    int      `json:"runtime"`
	Status     string   `json:"status"`
	TestCounts struct {
		Passed  int `json:"passed"`
		Failed  int `json:"failed"`
		Skipped int `json:"skipped"`
		Todo    int `json:"todo"`
		Total   int `json:"total"`
	}
	Tests []struct {
		Name     string   `json:"name"`
		FullName []string `json:"fullName"`
		Runtime  int      `json:"runtime"`
		Status   string   `json:"status"`
		Errors   []struct {
			Passed   bool        `json:"passed"`
			Actual   interface{} `json:"actual"`
			Expected interface{} `json:"expected"`
			Stack    string      `json:"string"`
			Todo     bool        `json:"todo"`
		} `json:"errors"`
	} `json:"errors"`
}

type runTestResult struct {
	path    string
	runEnd  qunitRunEnd
	runtime time.Duration
}

func (r *runTestResult) Pass() bool {
	return r.runEnd.Status == "passed"
}

var colorDim = "\033[37m"
var colorBold = "\033[1m"
var colorGreen = "\033[32m"
var colorRed = "\033[31m"
var colorReset = "\033[0m"

func init() {
	if _, found := os.LookupEnv("NO_COLOR"); found {
		colorDim = ""
		colorBold = ""
		colorGreen = ""
		colorRed = ""
		colorReset = ""
	}
}

func (r *runTestResult) WriteResult(prefix string, w io.Writer) {
	path := strings.TrimPrefix(r.path, prefix)
	if r.Pass() {
		fmt.Fprintf(w, "%s✓ %d pass %v %s%s\n", colorGreen, r.runEnd.TestCounts.Passed, r.runtime.Truncate(time.Millisecond), path, colorReset)
	} else {
		fmt.Fprintf(w, "%s✗ %d fail %v %s%s\n", colorRed, r.runEnd.TestCounts.Failed, r.runtime.Truncate(time.Millisecond), path, colorReset)
	}
}

func runTests(ctx context.Context, host string, path string) (*runTestResult, error) {
	var start = time.Now()
	finished := make(chan *runTestResult, 1)
	tasks := chromedp.Tasks{
		ExposeFunc("HARNESS_RUN_END", func(s string) {
			r := &runTestResult{path: path}
			if err := json.Unmarshal([]byte(s), &r.runEnd); err != nil {
				log.Fatal("expected error decoding runEnd JSON:", err)
			}
			finished <- r
		}),
		chromedp.Navigate(host + "/test/" + path),
	}
	if err := chromedp.Run(ctx, tasks); err != nil {
		return nil, errors.WithStack(err)
	}
	result := <-finished
	result.runtime = time.Since(start)
	return result, nil
}

func merge[T any](v [][]T) []T {
	total := 0
	for _, a := range v {
		total += len(a)
	}
	final := make([]T, 0, total)
	for _, a := range v {
		final = append(final, a...)
	}
	return final
}

func findTests(a *args) ([]string, error) {
	defer timeit("findTests")()
	fsys := os.DirFS(a.Root)
	excludes := make([]string, len(a.Exclude))
	for i, ex := range a.Exclude {
		excludes[i] = filepath.ToSlash(ex)
	}
	type result struct {
		matches []string
		error   error
	}
	results := make(chan result)
	final := make(chan result)
	go func() {
		var err error
		collect := make([][]string, 0, len(a.Include))
		for r := range results {
			if err == nil && r.error != nil {
				err = r.error
			}
			collect = append(collect, r.matches)
		}
		final <- result{
			matches: merge(collect),
			error:   err,
		}
	}()
	var wg sync.WaitGroup
	wg.Add(len(a.Include))
	for _, pattern := range a.Include {
		pattern := filepath.ToSlash(pattern)
		go func() {
			defer wg.Done()
			var matches []string
			err := doublestar.GlobWalk(fsys, pattern, func(path string, d fs.DirEntry) error {
				for i, ex := range excludes {
					match, err := doublestar.Match(ex, path)
					if err != nil {
						return errors.WithMessagef(err, "invalid exclude %q", a.Exclude[i])
					}
					if match {
						if d.IsDir() {
							return fs.SkipDir
						}
						return nil
					}
				}
				if d.Type().IsRegular() {
					matches = append(matches, path)
				}
				return nil
			})
			results <- result{
				error:   err,
				matches: matches,
			}
		}()
	}
	wg.Wait()
	close(results)
	r := <-final
	return r.matches, r.error
}

var binStart = time.Now()

func run() error {
	pwd, _ := os.Getwd()
	a := &args{
		Root:     pwd,
		Timeout:  time.Minute,
		Parallel: runtime.NumCPU(),
	}
	opts.Parse(a)
	if len(a.Include) == 0 {
		a.Include = defaultInclude[:]
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	allocatorOptions := chromedp.DefaultExecAllocatorOptions[:]
	if a.Visible {
		allocatorOptions = append(allocatorOptions, HeadlessFalse)
	}
	ctx, cancel = chromedp.NewExecAllocator(ctx, allocatorOptions...)
	defer cancel()

	ctx, cancel = chromedp.NewContext(ctx)
	defer cancel()

	server, err := testServer(ctx, a)
	if err != nil {
		return err
	}

	host := "http://" + server.Addr

	// TODO: bootstrap and glob tests in parallel

	// one run is needed to bootstrap the window
	bootstrap := make(chan error)
	go func() {
		defer timeit("browser bootstrap")()
		bootstrap <- chromedp.Run(ctx, chromedp.Navigate("about:blank"))
	}()

	tests, err := findTests(a)
	if err != nil {
		return err
	}

	if err := <-bootstrap; err != nil {
		return errors.WithStack(err)
	}

	// strip until the top-most shared directory
	stripPrefix := longestCommonPrefix(tests)
	if stat, err := os.Stat(filepath.Join(a.Root, stripPrefix)); err != nil || !stat.IsDir() {
		stripPrefix = filepath.Dir(stripPrefix) + string(filepath.Separator)
	}

	results := make(chan *runTestResult)
	printer := make(chan struct{})
	go func() {
		defer close(printer)
		for r := range results {
			r.WriteResult(stripPrefix, os.Stdout)
		}
	}()

	var stats struct {
		fail int32
		pass int32
	}

	var wg sync.WaitGroup
	wg.Add(len(tests))
	for _, path := range tests {
		path := path
		ctx, cancel := chromedp.NewContext(ctx)
		go func() {
			defer wg.Done()
			if !a.KeepRunning {
				defer cancel()
			}
			result, err := runTests(ctx, host, path)
			if err != nil {
				log.Printf("expected error running test %q: %v\n", path, err)
				return
			}
			atomic.AddInt32(&stats.fail, int32(result.runEnd.TestCounts.Failed))
			atomic.AddInt32(&stats.pass, int32(result.runEnd.TestCounts.Passed))
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	<-printer

	if a.KeepRunning {
		fmt.Println("Keeping browser running as requested, press Ctrl-C to quit.")
		<-ctx.Done()
	}

	fmt.Fprintf(os.Stdout, "%s--\n", colorDim)
	if fail := atomic.LoadInt32(&stats.fail); fail == 0 {
		pass := atomic.LoadInt32(&stats.pass)
		fmt.Fprintf(os.Stdout, "%s%s✓ %d pass %s%s\n", colorBold, colorGreen, pass, msSince(binStart), colorReset)
	} else {
		fmt.Fprintf(os.Stdout, "%s%s✗ %d fail %s%s\n", colorBold, colorRed, fail, msSince(binStart), colorReset)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%+v\n", err)
		os.Exit(1)
	}
}

var indexHTML = template.Must(template.New("index").Parse(
	`<!doctype html>
<html>

<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width">
  <title>Test Suite</title>
  <link rel="icon" href="data:">
  <link rel="stylesheet" href="/qunit.css">
</head>

<body>
  <div id="qunit"></div>
  <div id="qunit-fixture"></div>
  <script src="/qunit.js"></script>
  <script>
    if (window.HARNESS_RUN_END) {
      QUnit.on('runEnd', runEnd => {
				window.HARNESS_RUN_END(JSON.stringify(runEnd))
			});
    }
  </script>
  <script type="module" src="{{.TestSrc}}"></script>
</body>

</html>
`))
