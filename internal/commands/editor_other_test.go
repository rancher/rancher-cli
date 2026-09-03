//go:build !windows

package commands

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigEditMissingEditorIsValidationNamingBothVars asserts the
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

	_, err := NewConfigEdit(dir).(runtime.ReadCommand).Execute(t.Context(), args{}, &fakePrompter{canPrompt: true})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "VISUAL")
	assert.Contains(t, ce.Message, "EDITOR")
	assert.Contains(t, ce.Message, config.FilePath(dir))
}

// TestDefaultEditorEmptyOffWindows asserts the non-Windows contract: no
// baked-in editor default, so resolveEditor's VISUAL/EDITOR validation
// path (tested above) stays reachable. The "notepad" value is the
// Windows build's concern (editor_windows.go), exercised by the Windows
// acceptance pass — it cannot execute here.
func TestDefaultEditorEmptyOffWindows(t *testing.T) {
	assert.Empty(t, defaultEditor())
}
