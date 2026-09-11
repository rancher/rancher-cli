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

// fishProtoArgv runs the v2 fish script with the given simulated commandline
// (opc words = allButLast, ct = currentWord). Skips if fish is not on PATH.
func fishProtoArgv(t *testing.T, opcWords []string, ctWord string) []string {
	t.Helper()
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish not found on PATH; skipping fish request-shape test")
	}

	scriptPath := filepath.Join(t.TempDir(), "fish.sh")
	if err := os.WriteFile(scriptPath, fishScript, 0o644); err != nil {
		t.Fatalf("writing fish.sh: %v", err)
	}
	binDir := t.TempDir()
	argvFile := filepath.Join(binDir, "argv")
	fake := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do printf '%%s\\n' \"$a\" >> %q; done\necho pod\n", argvFile)
	if err := os.WriteFile(filepath.Join(binDir, "rancher"), []byte(fake), 0o755); err != nil {
		t.Fatalf("writing fake rancher: %v", err)
	}

	// Build one "printf '%s\n' 'word'" line per opc token so that fish command
	// substitution (which splits on newlines) yields one element per word.
	// This also allows words that contain spaces to round-trip correctly.
	var opcLines strings.Builder
	for _, w := range opcWords {
		q := "'" + strings.ReplaceAll(w, "'", "\\'") + "'"
		fmt.Fprintf(&opcLines, "            printf '%%s\\n' %s\n", q)
	}
	ctQ := "'" + strings.ReplaceAll(ctWord, "'", "\\'") + "'"

	driver := fmt.Sprintf(`
set -x PATH %q $PATH
# Stub commandline so the script uses our injected values.
function commandline
    switch $argv[1]
        case -opc
%s        case -ct
            echo %s
    end
end
source %q
__rancher_perform_completion
`, binDir, opcLines.String(), ctQ, scriptPath)

	if out, err := exec.Command(fish, "-c", driver).CombinedOutput(); err != nil {
		t.Fatalf("fish driver: %v\n%s", err, out)
	}

	data, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("argv file not written (fake rancher never invoked?): %v", err)
	}
	return splitLines(strings.TrimRight(string(data), "\n"))
}

// TestFishProtoV2RequestShape verifies the v2 fish script request shape.
func TestFishProtoV2RequestShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		opcWords []string // commandline -opc
		ctWord   string   // commandline -ct
		wantArgs []string
	}{
		{
			name:     "cursor-after-space empty cur",
			opcWords: []string{"rancher", "get", "pod"},
			ctWord:   "",
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get", "pod",
				"--rancher-completion-cur=",
				"--generate-shell-completion",
			},
		},
		{
			name:     "hostile cur glob",
			opcWords: []string{"rancher", "get"},
			ctWord:   "p*",
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get",
				"--rancher-completion-cur=p*",
				"--generate-shell-completion",
			},
		},
		{
			name:     "ambiguity side A: flag-last partial cur",
			opcWords: []string{"rancher", "get", "--cluster"},
			ctWord:   "c-",
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get", "--cluster",
				"--rancher-completion-cur=c-",
				"--generate-shell-completion",
			},
		},
		{
			name:     "ambiguity side B: flag+value consumed empty cur",
			opcWords: []string{"rancher", "get", "--cluster", "c-test"},
			ctWord:   "",
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get", "--cluster", "c-test",
				"--rancher-completion-cur=",
				"--generate-shell-completion",
			},
		},
		{
			name:     "completed word with spaces (single AST token)",
			opcWords: []string{"rancher", "get", "a b c"},
			ctWord:   "",
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get", "a b c",
				"--rancher-completion-cur=",
				"--generate-shell-completion",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := fishProtoArgv(t, tc.opcWords, tc.ctWord)
			if !sliceEqual(got, tc.wantArgs) {
				t.Errorf("argv mismatch\n got:  %q\n want: %q", got, tc.wantArgs)
			}
		})
	}
}

// fishScriptOutput runs __rancher_perform_completion and returns the lines it
// prints to stdout. The fake rancher binary emits candidateLines verbatim.
// Skips if fish is not on PATH.
func fishScriptOutput(t *testing.T, opcWords []string, ctWord string, candidateLines []string) []string {
	t.Helper()
	fish, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish not found on PATH; skipping fish candidate-output test")
	}

	scriptPath := filepath.Join(t.TempDir(), "fish.sh")
	if err := os.WriteFile(scriptPath, fishScript, 0o644); err != nil {
		t.Fatalf("writing fish.sh: %v", err)
	}
	binDir := t.TempDir()

	// Build fake rancher that prints the candidate lines.
	var fakeBody strings.Builder
	fakeBody.WriteString("#!/bin/sh\n")
	for _, line := range candidateLines {
		fmt.Fprintf(&fakeBody, "printf '%%s\\n' %s\n", "'"+strings.ReplaceAll(line, "'", "'\\''"+"'")+"'")
	}
	if err := os.WriteFile(filepath.Join(binDir, "rancher"), []byte(fakeBody.String()), 0o755); err != nil {
		t.Fatalf("writing fake rancher: %v", err)
	}

	var opcLines strings.Builder
	for _, w := range opcWords {
		q := "'" + strings.ReplaceAll(w, "'", "\\'") + "'"
		fmt.Fprintf(&opcLines, "            printf '%%s\\n' %s\n", q)
	}
	ctQ := "'" + strings.ReplaceAll(ctWord, "'", "\\'") + "'"

	driver := fmt.Sprintf(`
set -x PATH %q $PATH
function commandline
    switch $argv[1]
        case -opc
%s        case -ct
            echo %s
    end
end
source %q
__rancher_perform_completion
`, binDir, opcLines.String(), ctQ, scriptPath)

	out, err := exec.Command(fish, "-c", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("fish driver: %v\n%s", err, out)
	}
	return splitLines(strings.TrimRight(string(out), "\n"))
}

// TestFishCandidatesRancherPrefixSurvive verifies that candidates whose names
// start with "rancher" (e.g. rancher-prod, rancher-operator-system) are not
// filtered out by the completion script. The prior broad "rancher*" guard
// dropped all such candidates.
func TestFishCandidatesRancherPrefixSurvive(t *testing.T) {
	t.Parallel()

	candidates := []string{
		"rancher-prod:production cluster",
		"rancher-operator:operator namespace",
		"other-cluster:another cluster",
	}
	got := fishScriptOutput(t, []string{"rancher", "get"}, "", candidates)

	for _, want := range []string{"rancher-prod", "rancher-operator", "other-cluster"} {
		found := false
		for _, line := range got {
			if strings.HasPrefix(line, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("candidate %q was not in output; got=%q", want, got)
		}
	}
}
