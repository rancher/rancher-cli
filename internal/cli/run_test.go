package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunWritesARefusedLineToTheProcessErrorStream asserts that a line
// refused before the command tree acts on it is written to the process's
// standard error when the root has no error writer: the error decodes from
// the envelope, VALIDATION at exit 2, with no panic. Standard error is
// replaced by a file. Not parallel: it replaces os.Stderr and sets the
// captured command line.
func TestRunWritesARefusedLineToTheProcessErrorStream(t *testing.T) {
	restoreRawArgv(t)
	orig := os.Stderr
	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	require.NoError(t, err)
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = orig
		_ = f.Close()
	})

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	require.Nil(t, root.Writer)
	require.Nil(t, root.ErrWriter)

	assert.Equal(t, 2, runMain(t, root, "get", "pods", "-", "x", "-o", "json"))
	written, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	ce := renderedError(t, "json", string(written))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, bareDashMessage(0, "x", "-o", "json"), ce.Message)
}

// TestRunSetsTheStreamsOfARootThatHasNone asserts that a root whose Reader,
// Writer and ErrWriter are nil gets the process's standard input, output and
// error from a run. The command line is a completion request that matches no
// candidate and is captured first, as a process does: nothing is written.
// Not parallel: it sets the process environment and the captured command
// line.
func TestRunSetsTheStreamsOfARootThatHasNone(t *testing.T) {
	exitEnv(t)
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	require.Nil(t, root.Reader)
	require.Nil(t, root.Writer)
	require.Nil(t, root.ErrWriter)

	args := []string{"rancher", protoSentinel, curPrefix + "zzz-no-such-word", completionFlag}
	CaptureArgv(args[1:])
	require.NoError(t, root.Run(t.Context(), args))
	assert.Same(t, os.Stdin, root.Reader)
	assert.Same(t, os.Stdout, root.Writer)
	assert.Same(t, os.Stderr, root.ErrWriter)
}

// TestRunWithNoArgumentsShowsTheRootHelpPage asserts that Run called with a
// nil or an empty argument list, with no program name to check, writes the
// help page of the root, byte for byte as testdata/help/rancher.txt records
// it, to standard output and nothing to standard error, and exits 0. Not
// parallel: it sets the process environment and the captured command line.
func TestRunWithNoArgumentsShowsTheRootHelpPage(t *testing.T) {
	goldenEnvironment(t)
	want := readGolden(t, filepath.Join(goldenHelpDir, goldenName(nil)+".txt"))
	for name, args := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			root, err := BuildRoot(goldenVersion)
			require.NoError(t, err)
			root.Writer, root.ErrWriter = &out, &errOut

			assert.Zero(t, Run(t.Context(), root, args))
			assert.Equal(t, want, out.String())
			assert.Empty(t, errOut.String())
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
