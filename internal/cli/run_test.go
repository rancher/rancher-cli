package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// TestRunRefusesALineBeforeUrfaveSetsTheWriters asserts that a line refused
// before urfave parses it is written to the package error writer Run sets
// on a root that has none: the error decodes from the envelope, VALIDATION
// at exit 2, with no panic. The package writer is replaced by a buffer, and
// not a pipe, since it keeps the value it had when the package loaded. Not
// parallel: it replaces the package writer and sets the captured command
// line.
func TestRunRefusesALineBeforeUrfaveSetsTheWriters(t *testing.T) {
	restoreRawArgv(t)
	orig := ucli.ErrWriter
	t.Cleanup(func() { ucli.ErrWriter = orig })
	var errOut bytes.Buffer
	ucli.ErrWriter = &errOut

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	require.Nil(t, root.Writer)
	require.Nil(t, root.ErrWriter)

	assert.Equal(t, 2, runMain(t, root, "get", "pods", "-", "x", "-o", "json"))
	ce := renderedError(t, "json", errOut.String())
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, bareDashMessage(0, "x", "-o", "json"), ce.Message)
}

// TestRunWithNoArgumentsRunsTheRootUnchanged asserts that Run called with a
// nil or an empty argument list, with no program name to check, does what
// root.Run does with it: the same exit code and the same output.
func TestRunWithNoArgumentsRunsTheRootUnchanged(t *testing.T) {
	restoreRawArgv(t)
	for name, args := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			var gotOut, gotErr, wantOut, wantErr bytes.Buffer
			got, err := BuildRoot("v-test")
			require.NoError(t, err)
			got.Writer, got.ErrWriter = &gotOut, &gotErr
			want, err := BuildRoot("v-test")
			require.NoError(t, err)
			want.Writer, want.ErrWriter = &wantOut, &wantErr

			assert.Equal(t, exitCodeOf(want, want.Run(t.Context(), args)), Run(t.Context(), got, args))
			assert.Equal(t, wantOut.String(), gotOut.String())
			assert.Equal(t, wantErr.String(), gotErr.String())
		})
	}
}

// TestRunLeavesACompletionRequestToCompletion asserts that a completion
// request whose line holds a word after a "-", a bool flag typed with "="
// and an empty value, or a bool value as its own word, is not refused by
// Run: nothing is written to stderr, and completion offers nothing, as it
// does on a line that is refused when it runs. Not parallel: it sets the
// process environment and the captured command line.
func TestRunLeavesACompletionRequestToCompletion(t *testing.T) {
	extraArgsEnv(t)
	for _, typed := range [][]string{
		{"get", "pods", "-", "x"},
		{"get", "pods", "--yes="},
		{"get", "pods", "--dry-run", "false"},
		{"-", "get", "pods"},
	} {
		root, err := BuildRoot("v-test")
		require.NoError(t, err)
		var out, errOut bytes.Buffer
		root.Writer, root.ErrWriter = &out, &errOut
		label := "rancher " + strings.Join(typed, " ") + " <TAB>"
		require.Zero(t, runCompletionRequest(t, root, typed, ""), label)
		assert.Empty(t, errOut.String(), "%s: stderr", label)
		assert.Empty(t, out.String(), "%s: candidates", label)
	}
}
