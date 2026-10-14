package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// reportTo runs ReportRunError for err on a root whose error writer is a
// buffer, with argv captured as the process would, and returns the exit
// code and what reached stderr.
func reportTo(t *testing.T, err error, argv ...string) (int, string) {
	t.Helper()
	root, buildErr := cli.BuildRoot("v-test")
	require.NoError(t, buildErr)
	var stderr bytes.Buffer
	root.ErrWriter = &stderr
	cli.CaptureArgv(argv)
	return cli.ReportRunError(root, err), stderr.String()
}

// TestReportRunError asserts the exit code and the stderr text for each
// shape of error Run can return: a message is rendered once as a GENERIC
// error in the requested output format with exit 1 (never a code urfave
// chose, such as its help error's 3), an empty message writes nothing and
// keeps its exit code, and a plain error exits 1. Not parallel: the
// renderer reads the captured argv.
func TestReportRunError(t *testing.T) {
	t.Cleanup(func() { cli.CaptureArgv(nil) })

	code, stderr := reportTo(t, ucli.Exit("No help topic for 'x'", 3))
	assert.Equal(t, 1, code, "urfave's own exit code must not reach the caller")
	assert.Equal(t, "error (GENERIC): No help topic for 'x'\n", stderr)

	code, stderr = reportTo(t, errors.New("boom"))
	assert.Equal(t, 1, code)
	assert.Equal(t, "error (GENERIC): boom\n", stderr)

	code, stderr = reportTo(t, ucli.Exit("", 5))
	assert.Equal(t, 5, code)
	assert.Empty(t, stderr)

	code, stderr = reportTo(t, ucli.Exit("No help topic for 'x'", 3), "-o", "json", "get", "pod")
	assert.Equal(t, 1, code)
	var env struct {
		Data struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stderr), &env))
	assert.Equal(t, "GENERIC", env.Data.Error.Code)
	assert.Equal(t, "No help topic for 'x'", env.Data.Error.Message)
}

// TestRenderedErrorReachesMainWithEmptyMessage asserts the premise that
// keeps ReportRunError from printing twice: a command failure the pipeline has
// already rendered to stderr returns from Run with an empty message.
func TestRenderedErrorReachesMainWithEmptyMessage(t *testing.T) {
	t.Parallel()
	root, err := cli.BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

	runErr := root.Run(t.Context(), []string{"rancher", "get", "pod", "--no-such-flag"})
	require.Error(t, runErr)
	assert.Contains(t, errOut.String(), "no-such-flag")
	assert.Empty(t, runErr.Error(), "a rendered error must carry no message")
	var w bytes.Buffer
	root.ErrWriter = &w
	assert.Equal(t, 2, cli.ReportRunError(root, runErr))
	assert.Empty(t, w.String())
}

type exitSignal struct{ code int }

// runAsMain runs argv the way main does and returns everything written to
// stderr. A call to urfave's exit function ends the run with a panic that
// is recovered here, standing in for the process exiting: nothing after it
// (including the backstop) runs, as in a real process.
func runAsMain(t *testing.T, argv ...string) string {
	t.Helper()
	root, err := cli.BuildRoot("v-test")
	require.NoError(t, err)
	var stderr bytes.Buffer
	root.Writer = &bytes.Buffer{}
	root.ErrWriter = &stderr
	cli.CaptureArgv(argv)
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(exitSignal); !ok {
					panic(r)
				}
			}
		}()
		if runErr := root.Run(t.Context(), append([]string{"rancher"}, argv...)); runErr != nil {
			cli.ReportRunError(root, runErr)
		}
	}()
	return stderr.String()
}

// TestFailingInvocationsWriteTheirErrorOnce asserts that for each failing
// invocation the error text reaches stderr exactly once, counting what
// urfave writes, what the command pipeline renders and what main's
// backstop writes. Not parallel: it replaces urfave's exit function and
// error writer.
func TestFailingInvocationsWriteTheirErrorOnce(t *testing.T) {
	origExiter, origWriter := ucli.OsExiter, ucli.ErrWriter
	t.Cleanup(func() {
		ucli.OsExiter, ucli.ErrWriter = origExiter, origWriter
		cli.CaptureArgv(nil)
	})
	ucli.OsExiter = func(code int) { panic(exitSignal{code}) }

	const flagErr = "flag provided but not defined"
	tests := []struct {
		name   string
		argv   []string
		needle string
		want   int
	}{
		{"unknown flag on a leaf", []string{"get", "pod", "--bogus"}, flagErr, 1},
		{"unknown flag on a group", []string{"get", "--bogus"}, flagErr, 1},
		{"unknown flag on the root", []string{"--bogus"}, flagErr, 1},
		{"unknown command before an unknown flag", []string{"help", "--bogus"}, `unknown command "help"`, 1},
		{"unknown flag on the hidden completion command", []string{"__completion", "--bogus"}, flagErr, 1},
		{"unknown top-level command", []string{"nosuch"}, `unknown command "nosuch"`, 1},
		{"unknown command in a group", []string{"get", "nosuch"}, `unknown command "nosuch"`, 1},
		{"unknown name before --help on a group", []string{"get", "x", "--help"}, `unknown command "x"`, 1},
		{"help followed by a path", []string{"help", "get", "nosuch"}, `unknown command "help"`, 1},
		{"help inside a group", []string{"get", "help"}, `unknown command "help"`, 1},
		{"argument before --help on a leaf", []string{"get", "pod", "foo", "--help"}, "No help topic", 0},
		{"argument named help before --help on a leaf", []string{"get", "pod", "help", "--help"}, flagErr, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			ucli.ErrWriter = &stderr
			got := runAsMain(t, tt.argv...) + stderr.String()
			assert.Equal(t, tt.want, strings.Count(got, tt.needle), "stderr:\n%s", got)
			assert.NotContains(t, got, "Incorrect Usage")
			assert.NotContains(t, got, "error (GENERIC)", "nothing reaches the exit path backstop")
		})
	}
}
