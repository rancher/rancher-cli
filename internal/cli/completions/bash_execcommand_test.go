//go:build !windows

package completions

// Plain stdlib, like the other tests that run a real external shell.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
)

// TestBashExecCommandCompletion drives bash's completion callback with the
// real binary for kubeconfig --exec-command: a bare word gets the PATH
// executables that begin with it, every candidate beginning with the word,
// so bash's own filter keeps them and it never falls back to file names;
// a path word gets no candidate, so bash's -o default runs its file
// completion instead. Skips where bash is absent
// (fails instead when CI is set).
func TestBashExecCommandCompletion(t *testing.T) {
	bash := requireShell(t, "bash")
	binDir := t.TempDir()
	build := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(binDir, "rancher"), "github.com/rancher/rancher-cli/cmd/rancher")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building rancher: %s", out)
	}
	tools := t.TempDir()
	for _, n := range []string{"zzrancher-a", "zzrancher-b"} {
		testfs.WriteExecutable(t, filepath.Join(tools, n), []byte("#!/bin/sh\n"))
	}
	scriptPath := filepath.Join(t.TempDir(), "bash.sh")
	if err := os.WriteFile(scriptPath, bashScript, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		word string
		want string
	}{
		{"zzranc", "zzrancher-a,zzrancher-b,"},
		{"zznothing", "zznothing,"},
		{"./zz", ""},
		{"~/zz", ""},
	} {
		driver := fmt.Sprintf(`
set +e
export PATH=%q:%q:"$PATH"
export RANCHER_CLI_CONFIG_DIR=%q RANCHER_CLI_CREDENTIAL_STORE=file HOME=%q
_get_comp_words_by_ref() { cur="${COMP_WORDS[COMP_CWORD]}"; prev="${COMP_WORDS[COMP_CWORD-1]}"; words=("${COMP_WORDS[@]}"); cword="${COMP_CWORD}"; }
source %q
COMP_WORDS=(rancher kubeconfig --cluster c-1 --exec-command %s)
COMP_CWORD=5
COMP_TYPE=""
__rancher_bash_autocomplete
printf 'REPLY:'; for c in "${COMPREPLY[@]}"; do printf '%%s,' "${c%% }"; done; printf '\n'
`, binDir, tools, t.TempDir(), t.TempDir(), scriptPath, "'"+tc.word+"'")
		out, err := exec.CommandContext(t.Context(), bash, "-c", driver).CombinedOutput()
		if err != nil {
			t.Fatalf("running bash driver: %v\n%s", err, out)
		}
		var reply string
		for line := range strings.SplitSeq(string(out), "\n") {
			if r, ok := strings.CutPrefix(strings.TrimSpace(line), "REPLY:"); ok {
				reply = r
			}
		}
		if reply != tc.want {
			t.Errorf("--exec-command %s: COMPREPLY %q, want %q\n%s", tc.word, reply, tc.want, out)
		}
	}
}
