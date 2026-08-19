//go:build !windows

package completions

// Deliberately plain-stdlib (no testify): this test executes under a real
// external bash and is kept dependency-light on purpose.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestBashCompletionScriptExecutesSafely drives the FULL production callback
// __rancher_bash_autocomplete (not the request builder in isolation, so a
// regression that reintroduced eval inside the callback would be caught) with
// a $(...) payload sitting in a completion word. The rancher it invokes is a
// fake that RECORDS its exact argv, so the test proves the payload reached the
// binary as ONE literal argument - a marker-absence check alone would also
// pass if the word were dropped, split, or the command simply failed to run.
// It asserts three things: the completer still returns candidates, no marker
// file was created (nothing executed), and the recorded argv contains the
// payload verbatim as a single element. A minimal _get_comp_words_by_ref stub
// stands in for the bash-completion package a bare environment need not have;
// it only resolves cur/prev/words/cword and cannot touch the injection
// surface. Skips only where bash itself is absent.
func TestBashCompletionScriptExecutesSafely(t *testing.T) {
	t.Parallel()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found on PATH; skipping live bash completion execution test")
	}

	scriptPath := filepath.Join(t.TempDir(), "bash.sh")
	if err := os.WriteFile(scriptPath, bashScript, 0o644); err != nil {
		t.Fatalf("writing bash.sh: %v", err)
	}
	work := t.TempDir()
	marker := filepath.Join(work, "pwn_marker")
	argvFile := filepath.Join(work, "argv")
	payload := fmt.Sprintf("$(touch %s)", marker)

	// A fake rancher that appends each received argument, one per line, to
	// argvFile, then emits candidates so completion still returns something.
	fakeDir := t.TempDir()
	fake := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do printf '%%s\\n' \"$a\" >> %q; done\nprintf '%%s\\n' 'pod' 'pods'\n", argvFile)
	if err := os.WriteFile(filepath.Join(fakeDir, "rancher"), []byte(fake), 0o755); err != nil {
		t.Fatalf("writing fake rancher: %v", err)
	}

	driver := fmt.Sprintf(`
set +e
export PATH=%q:"$PATH"
_get_comp_words_by_ref() { cur="${COMP_WORDS[COMP_CWORD]}"; prev="${COMP_WORDS[COMP_CWORD-1]}"; words=("${COMP_WORDS[@]}"); cword="${COMP_CWORD}"; }
source %q

rm -f %q
COMP_WORDS=(rancher get '$(touch %s)' "")
COMP_CWORD=3
COMP_TYPE=""
__rancher_bash_autocomplete
printf 'REPLY:%%s\n' "$(printf '%%s,' "${COMPREPLY[@]}")"
printf 'MARKER:%%s\n' "$([ -e %q ] && echo true || echo false)"
`, fakeDir, scriptPath, marker, marker, marker)

	out, err := exec.Command(bash, "-c", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("running bash driver: %v\noutput:\n%s", err, out)
	}

	var reply string
	var markerCreated bool
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "REPLY:"):
			reply = strings.TrimPrefix(line, "REPLY:")
		case strings.HasPrefix(line, "MARKER:"):
			markerCreated, _ = strconv.ParseBool(strings.TrimPrefix(line, "MARKER:"))
		}
	}

	if len(splitNonEmpty(reply)) == 0 {
		t.Errorf("expected completion candidates, got none; full output:\n%s", out)
	}
	if markerCreated {
		t.Errorf("injection marker was created: the completer executed attacker-controlled text instead of passing it as a literal argument; full output:\n%s", out)
	}

	recorded, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("reading recorded argv (the fake rancher was never invoked?): %v\nfull output:\n%s", err, out)
	}
	args := splitLines(string(recorded))
	if !containsExact(args, payload) {
		t.Errorf("payload did not reach the binary as one literal argument; recorded argv=%q, want an element == %q; full output:\n%s", args, payload, out)
	}
}

func splitLines(s string) []string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func containsExact(xs []string, want string) bool {
	return slices.Contains(xs, want)
}

// TestBashCompletionDoesNotExecuteServerCandidates guards the second
// completion-time eval surface: the candidate list the emitted script filters
// by prefix. A candidate can carry attacker-influenced text - a config
// context/server name flows through the server-side completer back into the
// candidate list - so the script must match candidates literally, never let
// `compgen -W` word-split and command-substitute them. It drives the full
// __rancher_bash_autocomplete against a fake `rancher` that emits a $(...)
// candidate, with a minimal _get_comp_words_by_ref stub so no bash-completion
// package is required, and asserts the candidate is offered literally without
// executing. Skips only where bash is absent.
func TestBashCompletionDoesNotExecuteServerCandidates(t *testing.T) {
	t.Parallel()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found on PATH; skipping live bash completion execution test")
	}

	scriptPath := filepath.Join(t.TempDir(), "bash.sh")
	if err := os.WriteFile(scriptPath, bashScript, 0o644); err != nil {
		t.Fatalf("writing bash.sh: %v", err)
	}
	work := t.TempDir()
	evil := filepath.Join(work, "candidate_marker")

	fakeDir := t.TempDir()
	fake := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' '$(touch %s)' 'foo'\n", evil)
	if err := os.WriteFile(filepath.Join(fakeDir, "rancher"), []byte(fake), 0o755); err != nil {
		t.Fatalf("writing fake rancher: %v", err)
	}

	driver := fmt.Sprintf(`
set +e
export PATH=%q:"$PATH"
_get_comp_words_by_ref() { cur="${COMP_WORDS[COMP_CWORD]}"; prev="${COMP_WORDS[COMP_CWORD-1]}"; words=("${COMP_WORDS[@]}"); cword="${COMP_CWORD}"; }
source %q

rm -f %q
COMP_WORDS=(rancher config use-context "")
COMP_CWORD=3
COMP_TYPE=""
__rancher_bash_autocomplete
printf 'REPLY:%%s\n' "$(printf '%%s|' "${COMPREPLY[@]}")"
printf 'EVIL:%%s\n' "$([ -e %q ] && echo true || echo false)"
`, fakeDir, scriptPath, evil, evil)

	out, err := exec.Command(bash, "-c", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("running bash driver: %v\noutput:\n%s", err, out)
	}

	var reply string
	var evilCreated bool
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "REPLY:"):
			reply = strings.TrimPrefix(line, "REPLY:")
		case strings.HasPrefix(line, "EVIL:"):
			evilCreated, _ = strconv.ParseBool(strings.TrimPrefix(line, "EVIL:"))
		}
	}

	if evilCreated {
		t.Errorf("a $(...) completion candidate executed on filtering: candidates must be matched literally, not word-split by compgen; full output:\n%s", out)
	}
	if !strings.Contains(reply, "$(touch") {
		t.Errorf("expected the literal $(...) candidate offered as a prefix match, got REPLY %q; full output:\n%s", reply, out)
	}
}
