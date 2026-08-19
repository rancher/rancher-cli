package commands

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateCompletionGoldens = flag.Bool("update", false, "rewrite golden files")

type fakeInput struct {
	args []string
}

func (f fakeInput) String(string) string    { return "" }
func (f fakeInput) Bool(string) bool        { return false }
func (f fakeInput) Strings(string) []string { return nil }
func (f fakeInput) IsSet(string) bool       { return false }
func (f fakeInput) Args() []string          { return f.args }

// TestCompletionEmitsGoldenScriptPerShell pins each script byte-exact: these
// are install artifacts (sourced directly into a shell), so any drift —
// intended or not — must be a deliberate, reviewed diff against the golden,
// not silently absorbed by a substring assertion.
func TestCompletionEmitsGoldenScriptPerShell(t *testing.T) {
	for _, shell := range runtime.ShellCompletionShells() {
		t.Run(shell, func(t *testing.T) {
			res, err := NewCompletion().(completionCmd).Execute(t.Context(), fakeInput{args: []string{shell}}, nil)
			require.NoError(t, err)
			require.NotNil(t, res.RawBytes)

			golden := filepath.Join("testdata", "completion", shell+".golden")
			if *updateCompletionGoldens {
				require.NoError(t, os.WriteFile(golden, res.RawBytes, 0o644))
				return
			}
			want, err := os.ReadFile(golden)
			require.NoError(t, err)
			assert.Equal(t, string(want), string(res.RawBytes), "%s completion script drifted from golden; run with -update if intended", shell)
		})
	}
}

// TestCompletionUnknownShellIsValidation pins the generic Enum-validation
// path (external review finding 6): completion.go no longer hand-rolls this
// check, so the assertion must drive the full pipeline (runtime.Run) — its
// own Execute no longer validates the shell argument at all.
func TestCompletionUnknownShellIsValidation(t *testing.T) {
	t.Parallel()
	_, ce := runtime.Run(t.Context(), NewCompletion(), fakeInput{args: []string{"powershell-classic"}},
		runtime.IO{Out: io.Discard, Err: io.Discard}, safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "powershell-classic")
}

func TestCompletionCompleteArgsListsShells(t *testing.T) {
	t.Parallel()
	got := NewCompletion().(completionCmd).CompleteArgs(t.Context(), testView{})
	assert.Equal(t, runtime.ShellCompletionShells(), got)
}
