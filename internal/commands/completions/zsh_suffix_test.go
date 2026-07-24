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

// zshDescribeGroups runs _rancher with a stubbed _describe that records, for
// every candidate, which suffix group it was added under: "empty" when the
// _describe call carried the trailing compadd empty-suffix options (-S with
// an empty string), "default" otherwise. The fake rancher emits
// candidateLines verbatim. Returns "GROUP:<suffix>:<candidate>" lines.
// Skips if zsh is not on PATH.
func zshDescribeGroups(t *testing.T, words []string, candidateLines []string) []string {
	t.Helper()
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not found on PATH; skipping zsh suffix-group test")
	}

	scriptPath := filepath.Join(t.TempDir(), "zsh.sh")
	if err := os.WriteFile(scriptPath, zshScript, 0o644); err != nil {
		t.Fatalf("writing zsh.sh: %v", err)
	}

	binDir := t.TempDir()
	var fake strings.Builder
	fake.WriteString("#!/bin/sh\n")
	for _, line := range candidateLines {
		fmt.Fprintf(&fake, "printf '%%s\\n' '%s'\n", strings.ReplaceAll(line, "'", "'\\''"))
	}
	if err := os.WriteFile(filepath.Join(binDir, "rancher"), []byte(fake.String()), 0o755); err != nil {
		t.Fatalf("writing fake rancher: %v", err)
	}

	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = "'" + strings.ReplaceAll(w, "'", "'\\''") + "'"
	}
	wordsLiteral := "(" + strings.Join(quoted, " ") + ")"

	driver := fmt.Sprintf(`
export PATH=%q:"$PATH"
_describe() {
  local tag="$1" name="$2"
  shift 2
  local suffix="default"
  if [[ "$1" == "-S" && "$2" == "" ]]; then suffix="empty"; fi
  local item
  for item in "${(@P)name}"; do
    print -r -- "GROUP:${suffix}:${item}"
  done
}
_files() { print -r -- "FILES" }
source %q
words=%s
_rancher
`, binDir, scriptPath, wordsLiteral)

	out, err := exec.Command(zsh, "-c", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("zsh driver: %v\n%s", err, out)
	}
	return splitLines(strings.TrimRight(string(out), "\n"))
}

// TestZshContinuationCandidatesGetEmptySuffix pins the zsh side of the
// nospace handling: candidates whose token ends in "/" (the machine-config
// `<kind>/` prefix stage) or "=" (the template= output form) must be added
// with an empty compadd suffix (-S with an empty string) so acceptance does
// not append the space zsh adds by default — the next keystroke keeps
// extending the same word. Finished candidates keep the default suffix
// (a space).
func TestZshContinuationCandidatesGetEmptySuffix(t *testing.T) {
	t.Parallel()

	candidates := []string{
		"amazonec2config/",
		"template=",
		"pod:a pod",
	}
	got := zshDescribeGroups(t, []string{"rancher", "create", "cluster", "--machine-config", ""}, candidates)

	want := []string{
		"GROUP:empty:amazonec2config/",
		"GROUP:empty:template=",
		"GROUP:default:pod:a pod",
	}
	for _, w := range want {
		if !containsExact(got, w) {
			t.Errorf("missing %q in _describe groups; got=%q", w, got)
		}
	}
	for _, unwanted := range []string{
		"GROUP:default:amazonec2config/",
		"GROUP:default:template=",
		"GROUP:empty:pod:a pod",
	} {
		if containsExact(got, unwanted) {
			t.Errorf("candidate landed in the wrong suffix group: %q; got=%q", unwanted, got)
		}
	}
}
