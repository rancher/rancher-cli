//go:build !windows

package completions

// Deliberately plain-stdlib (no testify): these tests execute under a real
// external zsh and are kept dependency-light on purpose.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestZshHandsTheWholeLineToDescribe asserts the script passes every
// candidate line, description included, to _describe, which shows the text
// after the first colon as the description and inserts the token; a colon
// inside a description is kept. A candidate is a continuation (added with
// an empty suffix) by its token alone: a description that ends in a slash
// or an equals sign does not make it one, and a colon inside the
// description does not move the end of the token.
func TestZshHandsTheWholeLineToDescribe(t *testing.T) {
	t.Parallel()
	zsh := requireShell(t, "zsh")

	scriptPath := filepath.Join(t.TempDir(), "zsh.sh")
	if err := os.WriteFile(scriptPath, zshScript, 0o644); err != nil {
		t.Fatalf("writing zsh.sh: %v", err)
	}
	binDir := t.TempDir()
	lines := []string{
		"u-abc12:alice (Alice Smith)",
		"u-col01:dev: build team",
		"u-path:ends in a slash/",
		"u-eq:ends in an equals sign=",
		"u-plain",
		"amazonec2config/:machine configs",
		"template=:a Go template",
		"template=:a Go template: with a colon",
	}
	var fake strings.Builder
	fake.WriteString("#!/bin/sh\n")
	for _, l := range lines {
		fmt.Fprintf(&fake, "printf '%%s\\n' '%s'\n", strings.ReplaceAll(l, "'", "'\\''"))
	}
	if err := os.WriteFile(filepath.Join(binDir, "rancher"), []byte(fake.String()), 0o755); err != nil {
		t.Fatalf("writing fake rancher: %v", err)
	}

	// The stub prints which call it is (continuation when the suffix flag is
	// present) and the whole of each array element it was given.
	driver := fmt.Sprintf(`
export PATH=%q:"$PATH"
_describe() {
	local name=$2
	if [[ $3 == -S ]]; then print -r -- "continuing"; else print -r -- "finished"; fi
	print -rl -- "${(@P)name}"
}
_files() { : }
autoload -Uz compinit 2>/dev/null || true
source %q
words=(rancher get user '')
_rancher
`, binDir, scriptPath)

	// Standard output only: sourcing the script without the completion
	// system prints a compdef warning on standard error.
	out, err := exec.Command(zsh, "-c", driver).Output()
	if err != nil {
		t.Fatalf("zsh driver: %v\n%s", err, out)
	}
	want := []string{
		"continuing",
		"amazonec2config/:machine configs",
		"template=:a Go template",
		"template=:a Go template: with a colon",
		"finished",
		"u-abc12:alice (Alice Smith)",
		"u-col01:dev: build team",
		"u-path:ends in a slash/",
		"u-eq:ends in an equals sign=",
		"u-plain",
	}
	if got := splitLines(strings.TrimRight(string(out), "\n")); !sliceEqual(got, want) {
		t.Errorf("_describe calls\n got:  %q\n want: %q", got, want)
	}
}
