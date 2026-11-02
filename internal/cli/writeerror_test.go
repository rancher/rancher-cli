package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"

	"github.com/rancher/rancher-cli/internal/clierr"
)

var errScriptedWrite = errors.New("scripted write failure")

// scriptedErrWriter keeps the bytes of the calls it accepted. failFrom fails
// every call from that number on and failOnly fails that one call, both
// 1-based; a zero value of each turns it off.
type scriptedErrWriter struct {
	buf      bytes.Buffer
	calls    int
	failFrom int
	failOnly int
}

func (w *scriptedErrWriter) Write(p []byte) (int, error) {
	w.calls++
	if (w.failFrom > 0 && w.calls >= w.failFrom) || w.calls == w.failOnly {
		return 0, errScriptedWrite
	}
	return w.buf.Write(p)
}

// writeErrorLines are command lines that each end in an error the command
// line layer writes to standard error itself, one per place that writes it.
var writeErrorLines = []struct {
	name string
	argv []string
	env  map[string]string
	code string
}{
	{"a line refused after its --output was read", []string{"create", "namespace", "ns1", "--cluster", "local", "--apply=", "-o", "json"}, nil, clierr.CodeValidation},
	{"a flag value that is a flag, on schema", []string{"schema", "-o", "--color"}, nil, clierr.CodeValidation},
	{"a flag value that is a flag, on an engine command", []string{"access", "list", "--cluster", "local", "-o", "--color"}, nil, clierr.CodeValidation},
	{"an empty --config", []string{"--config", "", "config", "path"}, nil, clierr.CodeValidation},
	{"a credential store that cannot be resolved", []string{"-o", "json", "config", "path"}, map[string]string{"RANCHER_CLI_CREDENTIAL_STORE": "bogus"}, clierr.CodeGeneric},
	{"an unknown command", []string{"nosuchcommand"}, nil, clierr.CodeValidation},
	{"an unknown flag", []string{"access", "list", "--nosuchflag"}, nil, clierr.CodeValidation},
}

// runWithErrWriter runs argv through a new root as the process entry point
// does, with w as its error writer, and returns the exit code.
func runWithErrWriter(t *testing.T, argv []string, env map[string]string, w *scriptedErrWriter) int {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	root.ErrWriter = w
	root.Reader = strings.NewReader("")
	return runMain(t, root, argv...)
}

// TestAnErrorTheCommandLineCannotWriteIsWrittenAsOneLine asserts that for each
// place the command line layer writes an error, a standard error that fails
// its first write ends up holding exactly one line that names the error's
// code, and the exit code is the code's. Not parallel: it sets the process
// environment and the captured command line.
func TestAnErrorTheCommandLineCannotWriteIsWrittenAsOneLine(t *testing.T) {
	restoreRawArgv(t)
	extraArgsEnv(t)
	for _, tc := range writeErrorLines {
		t.Run(tc.name, func(t *testing.T) {
			w := &scriptedErrWriter{failOnly: 1}
			code := runWithErrWriter(t, tc.argv, tc.env, w)
			assert.Equal(t, clierr.ExitCode(tc.code), code)
			got := w.buf.String()
			assert.True(t, strings.HasPrefix(got, "error ("+tc.code+"): "), "stderr: %q", got)
			assert.Equal(t, 1, strings.Count(got, "\n"), "stderr: %q", got)
			assert.True(t, strings.HasSuffix(got, "\n"), "stderr: %q", got)
		})
	}
}

// TestAnErrorTheCommandLineCannotWriteKeepsItsExitCode asserts that for each
// place the command line layer writes an error, a standard error that fails
// every write leaves the exit code as the code's own and nothing in it. Not
// parallel: it sets the process environment and the captured command line.
func TestAnErrorTheCommandLineCannotWriteKeepsItsExitCode(t *testing.T) {
	restoreRawArgv(t)
	extraArgsEnv(t)
	for _, tc := range writeErrorLines {
		t.Run(tc.name, func(t *testing.T) {
			w := &scriptedErrWriter{failFrom: 1}
			code := runWithErrWriter(t, tc.argv, tc.env, w)
			assert.Equal(t, clierr.ExitCode(tc.code), code)
			assert.Zero(t, w.buf.Len())
		})
	}
}

// TestAHelpCardThatCannotBeWrittenExitsWithAnError asserts that a root card, a
// group card and a leaf card whose write to standard output fails end the run
// with exit code 1 and a GENERIC error on standard error that names the write
// error. Not parallel: it sets the process environment and the captured
// command line.
func TestAHelpCardThatCannotBeWrittenExitsWithAnError(t *testing.T) {
	restoreRawArgv(t)
	extraArgsEnv(t)
	for _, argv := range [][]string{
		{},
		{"--help"},
		{"access"},
		{"access", "--help"},
		{"access", "list", "--help"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			out := &scriptedErrWriter{failFrom: 1}
			var errOut bytes.Buffer
			root.Writer = out
			root.ErrWriter = &errOut
			root.Reader = strings.NewReader("")
			code := runMain(t, root, argv...)
			assert.Equal(t, 1, code)
			assert.Zero(t, out.buf.Len())
			assert.True(t, strings.HasPrefix(errOut.String(), "error (GENERIC): "), "stderr: %q", errOut.String())
			assert.Contains(t, errOut.String(), errScriptedWrite.Error())
		})
	}
}

// TestAVersionThatCannotBeWrittenExitsWithAnError asserts that --version with
// a standard output whose write fails ends the run with exit code 1 and a
// GENERIC error on standard error. Not parallel: it sets the process
// environment and the captured command line.
func TestAVersionThatCannotBeWrittenExitsWithAnError(t *testing.T) {
	restoreRawArgv(t)
	extraArgsEnv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	out := &scriptedErrWriter{failFrom: 1}
	var errOut bytes.Buffer
	root.Writer = out
	root.ErrWriter = &errOut
	root.Reader = strings.NewReader("")
	assert.Equal(t, 1, runMain(t, root, "--version"))
	assert.True(t, strings.HasPrefix(errOut.String(), "error (GENERIC): "), "stderr: %q", errOut.String())
	assert.Contains(t, errOut.String(), errScriptedWrite.Error())
}

// TestAGoodHelpRunNeverTakesTheWriteErrorOfAnotherRun asserts that help runs of
// two roots at the same time keep their own write errors: the run whose writer
// works ends without a kept error every time while the other fails and keeps
// its error until the end. Each run is root.Run; run itself reads
// process-wide state and is not for parallel use. Not parallel with other
// tests: it sets the process environment.
func TestAGoodHelpRunNeverTakesTheWriteErrorOfAnotherRun(t *testing.T) {
	restoreRawArgv(t)
	extraArgsEnv(t)
	const runs = 200
	build := func(w io.Writer) []*ucli.Command {
		roots := make([]*ucli.Command, runs)
		for i := range roots {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			root.Reader = strings.NewReader("")
			root.Writer = w
			root.ErrWriter = &bytes.Buffer{}
			roots[i] = root
		}
		return roots
	}
	failing := build(&scriptedErrWriter{failFrom: 1})
	working := build(&bytes.Buffer{})
	start := make(chan struct{})
	codes := make([]int, runs)
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		for _, root := range failing {
			require.NoError(t, root.Run(t.Context(), []string{"rancher", "--help"}))
		}
	})
	wg.Go(func() {
		<-start
		for i, root := range working {
			codes[i] = helpOf(t, root)
		}
	})
	close(start)
	wg.Wait()
	for i, code := range codes {
		assert.Zero(t, code, "run %d", i)
	}
	for i, root := range failing {
		assert.Error(t, takeHelpWriteError(root), "failing run %d keeps its own error", i)
	}
}

// helpOf shows the help card of root with root.Run, which does not read or
// write the captured command line.
func helpOf(t *testing.T, root *ucli.Command) int {
	t.Helper()
	if err := root.Run(t.Context(), []string{"rancher", "--help"}); err != nil {
		return 1
	}
	if takeHelpWriteError(root) != nil {
		return 1
	}
	return 0
}
