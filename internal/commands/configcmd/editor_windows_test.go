//go:build windows

package configcmd

// Deliberately plain-stdlib (no testify): this file only compiles/runs on
// Windows builds and stays minimal.

import (
	"testing"
)

// TestResolveEditorFallsBackToNotepadOnWindows asserts the Windows-only
// positive contract that makes the non-Windows validation-error test
// (config_edit_test.go) inapplicable there: with both VISUAL and EDITOR
// unset, resolveEditor must resolve to notepad, not fail.
func TestResolveEditorFallsBackToNotepadOnWindows(t *testing.T) {
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	bin, args, ok := resolveEditor()
	if !ok || bin != "notepad" || len(args) != 0 {
		t.Fatalf("resolveEditor() = %q, %v, %v; want \"notepad\", [], true", bin, args, ok)
	}
}
