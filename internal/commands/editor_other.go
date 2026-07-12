//go:build !windows

package commands

// defaultEditor returns the editor used when neither $VISUAL nor $EDITOR
// is set. Empty on non-Windows: the validation error naming both vars
// (resolveEditor's ok=false path) is deliberate, tested behavior.
func defaultEditor() string { return "" }
