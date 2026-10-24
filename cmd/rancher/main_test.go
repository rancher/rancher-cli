package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// TestRenderedErrorReachesMainWithEmptyMessage asserts that a command
// failure the pipeline has already rendered to stderr is not rendered
// again: Run returns its exit code and the error text reaches stderr once.
func TestRenderedErrorReachesMainWithEmptyMessage(t *testing.T) {
	t.Cleanup(func() { cli.CaptureArgv(nil) })
	root, err := cli.BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut

	code := cli.Run(t.Context(), root, []string{"rancher", "get", "pod", "--no-such-flag"})
	assert.Equal(t, 2, code)
	assert.Equal(t, 1, strings.Count(errOut.String(), "no-such-flag"), "stderr:\n%s", errOut.String())
	assert.NotContains(t, errOut.String(), "error (GENERIC)")
}

// runAsMain runs argv the way main does and returns everything written to
// stderr and the exit code Run returned.
func runAsMain(t *testing.T, argv ...string) (string, int) {
	t.Helper()
	root, err := cli.BuildRoot("v-test")
	require.NoError(t, err)
	var stderr bytes.Buffer
	root.Writer = &bytes.Buffer{}
	root.ErrWriter = &stderr
	code := cli.Run(t.Context(), root, append([]string{"rancher"}, argv...))
	return stderr.String(), code
}

// TestFailingInvocationsWriteTheirErrorOnce asserts that for each failing
// invocation the error text reaches stderr exactly once, counting what
// urfave writes, what the command pipeline renders and what Run writes for
// an error nothing rendered. Not parallel: it replaces urfave's error
// writer.
func TestFailingInvocationsWriteTheirErrorOnce(t *testing.T) {
	origWriter := ucli.ErrWriter
	t.Cleanup(func() {
		ucli.ErrWriter = origWriter
		cli.CaptureArgv(nil)
	})

	const flagErr = "flag provided but not defined"
	tests := []struct {
		name   string
		argv   []string
		needle string
		want   int
	}{
		{"unknown flag on a leaf", []string{"get", "pod", "--bogus"}, flagErr, 1},
		{"unknown --wait on delete", []string{"delete", "pod", "web", "--wait"}, flagErr, 1},
		{"unknown --for on patch", []string{"patch", "pod", "web", "--for", "x"}, flagErr, 1},
		{"unknown --timeout on the clusterregistrationtoken get", []string{"get", "clusterregistrationtoken", "x", "--timeout", "1s"}, flagErr, 1},
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
			runErr, code := runAsMain(t, tt.argv...)
			got := runErr + stderr.String()
			wantCode := 2
			if tt.want == 0 {
				wantCode = 0
			}
			assert.Equal(t, wantCode, code)
			assert.Equal(t, tt.want, strings.Count(got, tt.needle), "stderr:\n%s", got)
			assert.NotContains(t, got, "Incorrect Usage")
			assert.NotContains(t, got, "error (GENERIC)", "nothing reaches Run's rendering of an unrendered error")
		})
	}
}
