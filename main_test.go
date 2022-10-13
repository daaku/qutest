package main_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/daaku/ensure"
)

func run(t testing.TB, args ...string) (string, int) {
	bin := "." + string(filepath.Separator) + "qutest"
	_, err := os.Stat(bin)
	ensure.Nil(t, err, "remember to go install before running tests")

	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "NO_COLOR")
	out, err := cmd.CombinedOutput()
	exit := 0
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		exit = exitError.ProcessState.ExitCode()
	} else {
		ensure.Nil(t, err, "unexpected error")
	}
	return string(out), exit
}

func TestSimpleSuccess(t *testing.T) {
	t.Parallel()
	out, exit := run(t, "tests/should_pass.js")
	ensure.DeepEqual(t, exit, 0)
	ensure.StringContains(t, out, "1 pass")
}

func TestSimpleFailure(t *testing.T) {
	t.Parallel()
	out, exit := run(t, "tests/should_fail.js")
	ensure.DeepEqual(t, exit, 1)
	ensure.StringContains(t, out, "1 fail")
}

func TestTypescriptSuccess(t *testing.T) {
	t.Parallel()
	out, exit := run(t, "tests/a_typescript_file.ts")
	ensure.DeepEqual(t, exit, 0)
	ensure.StringContains(t, out, "1 pass")
}
