package completions

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestBashCompletionScriptExecutesSafely shells out to a real bash to run the
// emitted bash.sh exactly where the command-injection lived: it sources the
// script, drives __rancher_build_completion_request + the argv-array
// invocation directly (the two lines the fix changed), and asserts a $(...)
// payload sitting in a completion word is passed as an inert argument rather
// than executed. It deliberately does NOT route through
// __rancher_init_completion, which depends on the bash-completion package a
// bare environment need not have - the security-relevant surface is the
// request build + invocation, and that is what this exercises. A third case
// reproduces the old `eval` form and asserts it WOULD have created the marker,
// so the test proves it can discriminate rather than passing vacuously. Skips
// only where bash itself is absent.
func TestBashCompletionScriptExecutesSafely(t *testing.T) {
	t.Parallel()

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found on PATH; skipping live bash completion execution test")
	}

	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "rancher")
	build := exec.Command("go", "build", "-o", binPath, "../../..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building rancher: %s", out)
	}

	scriptPath := filepath.Join(t.TempDir(), "bash.sh")
	if err := os.WriteFile(scriptPath, bashScript, 0o644); err != nil {
		t.Fatalf("writing bash.sh: %v", err)
	}
	work := t.TempDir()
	marker := filepath.Join(work, "pwn_marker")
	evalMarker := filepath.Join(work, "eval_marker")

	driver := fmt.Sprintf(`
set +e
export PATH=%q:"$PATH"
source %q

# Functional: a clean line still yields kind names.
COMP_WORDS=(rancher get "")
COMP_CWORD=2
__rancher_build_completion_request
kinds=$("${__rancher_completion_request[@]}" 2>/dev/null)
printf 'KINDS:%%s\n' "$(printf '%%s' "$kinds" | tr '\n' ',')"

# Injection safety: a word carrying $(...) must reach the binary as a
# literal argument, never be evaluated.
rm -f %q
COMP_WORDS=(rancher get '$(touch %q)' "")
COMP_CWORD=3
__rancher_build_completion_request
"${__rancher_completion_request[@]}" >/dev/null 2>&1
printf 'MARKER:%%s\n' "$([ -e %q ] && echo true || echo false)"

# Discrimination: the old eval form WOULD execute the payload.
rm -f %q
bad='rancher get $(touch %q) --generate-shell-completion'
eval "$bad" >/dev/null 2>&1
printf 'EVALMARKER:%%s\n' "$([ -e %q ] && echo true || echo false)"
`, binDir, scriptPath, marker, marker, marker, evalMarker, evalMarker, evalMarker)

	out, err := exec.Command(bash, "-c", driver).CombinedOutput()
	if err != nil {
		t.Fatalf("running bash driver: %v\noutput:\n%s", err, out)
	}

	var kinds []string
	var markerCreated, evalMarkerCreated bool
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "KINDS:"):
			kinds = splitNonEmpty(strings.TrimPrefix(line, "KINDS:"))
		case strings.HasPrefix(line, "MARKER:"):
			markerCreated, _ = strconv.ParseBool(strings.TrimPrefix(line, "MARKER:"))
		case strings.HasPrefix(line, "EVALMARKER:"):
			evalMarkerCreated, _ = strconv.ParseBool(strings.TrimPrefix(line, "EVALMARKER:"))
		}
	}

	if len(kinds) == 0 {
		t.Errorf("expected completion candidates for %q, got none; full output:\n%s", "rancher get ", out)
	}
	if markerCreated {
		t.Errorf("injection marker was created: the completer evaluated attacker-controlled text instead of passing it as a literal argument; full output:\n%s", out)
	}
	if !evalMarkerCreated {
		t.Errorf("discrimination check failed: the reference eval form did not create its marker, so a passing MARKER assertion would be vacuous; full output:\n%s", out)
	}
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
	for _, line := range strings.Split(string(out), "\n") {
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
