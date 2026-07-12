package commands

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateCompletionGoldens = flag.Bool("update", false, "rewrite golden files")

type fakeInput struct {
	args []string
}

func (f fakeInput) String(string) string { return "" }
func (f fakeInput) Bool(string) bool     { return false }
func (f fakeInput) Args() []string       { return f.args }

// TestCompletionEmitsGoldenScriptPerShell pins each script byte-exact: these
// are install artifacts (sourced directly into a shell), so any drift —
// intended or not — must be a deliberate, reviewed diff against the golden,
// not silently absorbed by a substring assertion.
func TestCompletionEmitsGoldenScriptPerShell(t *testing.T) {
	for _, shell := range runtime.ShellCompletionShells() {
		t.Run(shell, func(t *testing.T) {
			res, err := NewCompletion().(completionCmd).Execute(context.Background(), fakeInput{args: []string{shell}}, nil)
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

func TestCompletionUnknownShellIsValidation(t *testing.T) {
	t.Parallel()
	_, err := NewCompletion().(completionCmd).Execute(context.Background(), fakeInput{args: []string{"powershell-classic"}}, nil)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "powershell-classic")
}

func TestCompletionCompleteArgsListsShells(t *testing.T) {
	t.Parallel()
	got := NewCompletion().(completionCmd).CompleteArgs(context.Background(), "/ignored")
	assert.Equal(t, runtime.ShellCompletionShells(), got)
}
