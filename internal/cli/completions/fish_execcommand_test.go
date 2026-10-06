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

// TestFishOffersFilesOnlyForAnExecCommandPath asserts the fish script's
// one file rule fires for a path word after --exec-command - one with a /
// or a leading ~, in either the space or the = form - and for nothing
// else, so a bare word keeps the binary's PATH names. Skips without fish
// (fails instead when CI is set).
func TestFishOffersFilesOnlyForAnExecCommandPath(t *testing.T) {
	fish := requireShell(t, "fish")
	scriptPath := filepath.Join(t.TempDir(), "fish.sh")
	if err := os.WriteFile(scriptPath, fishScript, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		prev, cur string
		want      bool
	}{
		{"--exec-command", "./bin", true},
		{"--exec-command", "bin/r", true},
		{"--exec-command", "~", true},
		{"--exec-command", "~/b", true},
		{"--cluster", "--exec-command=~/b", true},
		{"--cluster", "--exec-command=./b", true},
		{"--exec-command", "ranc", false},
		{"--cluster", "--exec-command=ranc", false},
		{"--cluster", "./bin", false},
	} {
		driver := fmt.Sprintf(`
function commandline
    switch $argv[1]
        case -opc
            printf '%%s\n' rancher kubeconfig %s
        case -ct
            printf '%%s\n' %s
    end
end
source %q
__rancher_exec_command_path
`, tc.prev, "'"+strings.ReplaceAll(tc.cur, "'", `\'`)+"'", scriptPath)
		err := exec.CommandContext(t.Context(), fish, "-c", driver).Run()
		if got := err == nil; got != tc.want {
			t.Errorf("after %s, %q: file rule %v, want %v (%v)", tc.prev, tc.cur, got, tc.want, err)
		}
	}
}
