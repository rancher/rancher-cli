//go:build !windows

package commands

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigEditMissingEditorIsValidationNamingBothVars pins the
// non-Windows validation-error path: with neither $VISUAL nor $EDITOR
// set, config edit must fail naming both vars. Build-tagged !windows
// because defaultEditor() always returns "notepad" on Windows
// (editor_windows.go), making this path unreachable there — see
// TestResolveEditorFallsBackToNotepadOnWindows in editor_windows_test.go
// for the Windows-side positive contract.
func TestConfigEditMissingEditorIsValidationNamingBothVars(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	dir := t.TempDir()

	_, err := NewConfigEdit(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, &fakePrompter{canPrompt: true})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "VISUAL")
	assert.Contains(t, ce.Message, "EDITOR")
	assert.Contains(t, ce.Message, config.FilePath(dir))
}
