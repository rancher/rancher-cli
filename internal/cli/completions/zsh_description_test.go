//go:build !windows

package completions

// The assertions in these tests use the standard library only (no testify). These tests execute
// under a real external zsh. The stub writer comes from internal/testfs.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
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
		"project:Project namespace commands (managed with `rancher grant`)",
		"amazonec2config/:machine configs",
		"template=:a Go template",
		"template=:a Go template: with a colon",
	}
	var fake strings.Builder
	fake.WriteString("#!/bin/sh\n")
	for _, l := range lines {
		fmt.Fprintf(&fake, "printf '%%s\\n' '%s'\n", strings.ReplaceAll(l, "'", "'\\''"))
	}
	testfs.WriteExecutable(t, filepath.Join(binDir, "rancher"), []byte(fake.String()))

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
		"project:Project namespace commands (managed with `rancher grant`)",
	}
	if got := splitLines(strings.TrimRight(string(out), "\n")); !sliceEqual(got, want) {
		t.Errorf("_describe calls\n got:  %q\n want: %q", got, want)
	}
}
