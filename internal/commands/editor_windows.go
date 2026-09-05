//go:build windows

package commands

// defaultEditor returns notepad when neither $VISUAL nor $EDITOR is set:
// always present on Windows, and it blocks until the window closes, so
// the exec-and-wait edit loop works unchanged. Note: notepad may save
// CRLF line endings - YAML tolerates them and the file round-trips.
func defaultEditor() string { return "notepad" }
