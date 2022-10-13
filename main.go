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
	"syscall"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	cdruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/jpillora/opts"
	"github.com/pkg/errors"
)

//go:generate mkdir -p assets
//go:generate curl -o assets/qunit.css https://code.jquery.com/qunit/qunit-2.19.1.css
//go:generate curl -o assets/qunit.js https://code.jquery.com/qunit/qunit-2.19.1.js

//go:embed assets/qunit.css
var qunitCSS []byte

//go:embed assets/qunit.js
var qunitJS []byte

var defaultInclude = []string{"**/*.js", "**/*.ts", "**/*.jsx", "**/*.tsx"}

type args struct {
	Root        string        `opts:"help=root directory"`
	Include     []string      `opts:"mode=arg,help=globs to include"`
	Exclude     []string      `opts:"help=globs to exclude"`
	Coverage    bool          `opts:"help=enable code coverage"`
	Timeout     time.Duration `opts:"help=timeout for all tests"`
	Parallel    int           `opts:"help=number of parallel tests"`
	Watch       bool          `opts:"help=watch mode"`
	Visible     bool          `opts:"help=run visible browser"`
	KeepRunning bool          `opts:"help=keep browser running after tests"`
	Port        int           `opts:"help=use specific port for internal server"`
}

func testServer(ctx context.Context, args *args) (*http.Server, error) {
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
		src := strings.TrimPrefix(r.URL.Path, "/bundle")
		http.ServeFile(w, r, filepath.Join(args.Root, src))
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

var colorGreen = "\033[32m"
var colorRed = "\033[31m"
var colorReset = "\033[0m"

func init() {
	if _, found := os.LookupEnv("NO_COLOR"); found {
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
	if err := chromedp.Run(ctx, chromedp.Navigate("about:blank")); err != nil {
		return errors.WithStack(err)
	}

	tests, err := findTests(a)
	if err != nil {
		return err
	}
	stripPrefix := longestCommonPrefix(tests)

	results := make(chan *runTestResult)
	printer := make(chan struct{})
	go func() {
		defer close(printer)
		for r := range results {
			r.WriteResult(stripPrefix, os.Stdout)
		}
	}()

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

	log.Printf("Took %v.\n", time.Since(binStart).Truncate(time.Millisecond))
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
  <script src="{{.TestSrc}}"></script>
</body>

</html>
`))
