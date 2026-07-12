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
