//go:build windows

package token

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestOSOpenURLDoesNotShellInterpret guards the Windows RCE fix: osOpenURL must
// hand the URL to the ShellExecute API as an opaque string, never to a shell.
// The URL below carries a cmd.exe metacharacter (&) and a canary command; under
// the old `cmd /c start` path a shell would have split on & and run the canary,
// creating marker. ShellExecute cannot, so marker must never appear.
//
// Note: this opens the default browser to a dead 127.0.0.1 URL as a side effect
// (ShellExecute "open" on an http URL) — expected, harmless. Windows-only.
func TestOSOpenURLDoesNotShellInterpret(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned.txt")
	url := `https://127.0.0.1:9/?x=1& echo pwned> ` + marker
	_ = osOpenURL(url) // may error or open a browser tab; both are fine
	time.Sleep(750 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("canary file created — the URL was shell-interpreted (injection): %s", marker)
	}
}
