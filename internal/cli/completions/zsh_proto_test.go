//go:build !windows

package completions

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// zshProtoArgv runs the v2 zsh script with the given words array and returns
// the argv the recorder binary received. Skips if zsh is not on PATH.
func zshProtoArgv(t *testing.T, words []string) []string {
	t.Helper()
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not found on PATH; skipping zsh request-shape test")
	}

	scriptPath := filepath.Join(t.TempDir(), "zsh.sh")
	if err := os.WriteFile(scriptPath, zshScript, 0o644); err != nil {
		t.Fatalf("writing zsh.sh: %v", err)
	}
	binDir := t.TempDir()
	argvFile := filepath.Join(binDir, "argv")
	fake := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do printf '%%s\\n' \"$a\" >> %q; done\necho pod\n", argvFile)
	if err := os.WriteFile(filepath.Join(binDir, "rancher"), []byte(fake), 0o755); err != nil {
		t.Fatalf("writing fake rancher: %v", err)
	}

	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = "'" + strings.ReplaceAll(w, "'", "'\\''") + "'"
	}
	wordsLiteral := "(" + strings.Join(quoted, " ") + ")"

	driver := fmt.Sprintf(`
export PATH=%q:"$PATH"
_describe() { : }
_files() { : }
autoload -Uz compinit 2>/dev/null || true
source %q
words=%s
_rancher
`, binDir, scriptPath, wordsLiteral)

	if out, err := exec.Command(zsh, "-c", driver).CombinedOutput(); err != nil {
		t.Fatalf("zsh driver: %v\n%s", err, out)
	}

	data, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("argv file not written (fake rancher never invoked?): %v", err)
	}
	return splitLines(strings.TrimRight(string(data), "\n"))
}

// TestZshStderrNotLeakedToTerminal verifies the zsh script suppresses the
// binary's stderr the way fish.sh, bash.sh, and pwsh.ps1 already do - a
// binary that rejects the request (e.g. a stale install that predates the
// v2 protocol) must degrade to "no candidates", not splat errors onto the
// command line being edited.
func TestZshStderrNotLeakedToTerminal(t *testing.T) {
	t.Parallel()
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not found on PATH; skipping zsh stderr test")
	}

	scriptPath := filepath.Join(t.TempDir(), "zsh.sh")
	if err := os.WriteFile(scriptPath, zshScript, 0o644); err != nil {
		t.Fatalf("writing zsh.sh: %v", err)
	}
	binDir := t.TempDir()
	fake := "#!/bin/sh\necho 'boom: stale binary rejects completion flags' >&2\necho pod\n"
	if err := os.WriteFile(filepath.Join(binDir, "rancher"), []byte(fake), 0o755); err != nil {
		t.Fatalf("writing fake rancher: %v", err)
	}

	driver := fmt.Sprintf(`
export PATH=%q:"$PATH"
_describe() { : }
_files() { : }
autoload -Uz compinit 2>/dev/null || true
source %q
words=('rancher' 'get' '')
_rancher
`, binDir, scriptPath)

	out, err := exec.Command(zsh, "-c", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("zsh driver: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "boom") {
		t.Errorf("binary stderr leaked through the completion function:\n%s", out)
	}
}

// TestZshProtoV2RequestShape verifies the v2 zsh script request shape.
func TestZshProtoV2RequestShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		words    []string // zsh words array (index 1 = binary, last = current word)
		wantArgs []string
	}{
		{
			name:  "cursor-after-space empty cur",
			words: []string{"rancher", "get", "pod", ""},
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get", "pod",
				"--rancher-completion-cur=",
				"--generate-shell-completion",
			},
		},
		{
			name:  "hostile cur with spaces",
			words: []string{"rancher", "get", "a b"},
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get",
				"--rancher-completion-cur=a b",
				"--generate-shell-completion",
			},
		},
		{
			name:  "ambiguity side A: flag-last partial cur",
			words: []string{"rancher", "get", "--cluster", "c-"},
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get", "--cluster",
				"--rancher-completion-cur=c-",
				"--generate-shell-completion",
			},
		},
		{
			name:  "ambiguity side B: flag+value consumed empty cur",
			words: []string{"rancher", "get", "--cluster", "c-test", ""},
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get", "--cluster", "c-test",
				"--rancher-completion-cur=",
				"--generate-shell-completion",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := zshProtoArgv(t, tc.words)
			if !sliceEqual(got, tc.wantArgs) {
				t.Errorf("argv mismatch\n got:  %q\n want: %q", got, tc.wantArgs)
			}
		})
	}
}
