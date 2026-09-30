package completions

// Deliberately plain-stdlib (no testify): these tests execute under a real
// external PowerShell and are kept dependency-light on purpose.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// findPowerShell prefers pwsh (PowerShell 7+, cross-platform); Windows PowerShell
// 5.1 (powershell.exe) is the fallback so this still runs on older Windows images.
func findPowerShell(t *testing.T) (string, bool) {
	t.Helper()
	for _, name := range []string{"pwsh", "powershell"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, true
		}
	}
	return "", false
}

func psSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// TestPwshCompletionScriptExecutesSafely shells out to a real PowerShell to
// run the emitted pwsh.ps1 exactly as a user's shell would: registers it as
// a native argument completer, then drives TabExpansion2 (the same machinery
// a real TAB keypress invokes). It is the durable regression guard for the
// command-injection fix - a unit test alone can't prove the fix holds
// through PowerShell's own completer dispatch, only a live shell can. Skips
// only where neither pwsh nor powershell is reachable at all (a minimal
// container or self-hosted runner without either installed) - GitHub's
// hosted ubuntu-latest image ships pwsh by default, so this is expected to
// run rather than skip on this repo's own CI.
func TestPwshCompletionScriptExecutesSafely(t *testing.T) {
	shell, ok := findPowerShell(t)
	if !ok {
		t.Skip("neither pwsh nor powershell found on PATH; skipping live PowerShell completion execution test")
	}

	binDir := t.TempDir()
	binName := "rancher"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(binDir, binName)

	// The entry point by import path, so the build does not depend on how
	// deep this package sits.
	build := exec.Command("go", "build", "-o", binPath, "github.com/rancher/rancher-cli/cmd/rancher")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building rancher: %s", out)
	}

	scriptPath := filepath.Join(t.TempDir(), "pwsh.ps1")
	if err := os.WriteFile(scriptPath, pwshScript, 0o644); err != nil {
		t.Fatalf("writing pwsh.ps1: %v", err)
	}
	markerPath := filepath.Join(t.TempDir(), "pwn_marker")

	driver := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$env:PATH = %s + [System.IO.Path]::PathSeparator + $env:PATH
. %s

$line1 = 'rancher get '
$r1 = TabExpansion2 -inputScript $line1 -cursorColumn $line1.Length
Write-Output ('KINDS:' + (($r1.CompletionMatches | ForEach-Object { $_.CompletionText }) -join ','))

Remove-Item -ErrorAction SilentlyContinue %s
$payload = 'rancher get "$(New-Item -Path ' + %s + ' -ItemType File -Force)" po'
$r2 = TabExpansion2 -inputScript $payload -cursorColumn $payload.Length
Write-Output ('AFTERPAYLOAD:' + (($r2.CompletionMatches | ForEach-Object { $_.CompletionText }) -join ','))
Write-Output ('MARKER:' + (Test-Path %s))
`, psSingleQuote(binDir), psSingleQuote(scriptPath), psSingleQuote(markerPath), psSingleQuote(markerPath), psSingleQuote(markerPath))

	driverPath := filepath.Join(t.TempDir(), "driver.ps1")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("writing driver.ps1: %v", err)
	}

	cmd := exec.Command(shell, "-NoProfile", "-NonInteractive", "-File", driverPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running pwsh driver: %v\noutput:\n%s", err, out)
	}

	var kinds, afterPayload []string
	var markerCreated bool
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "KINDS:"):
			kinds = splitNonEmpty(strings.TrimPrefix(line, "KINDS:"))
		case strings.HasPrefix(line, "AFTERPAYLOAD:"):
			afterPayload = splitNonEmpty(strings.TrimPrefix(line, "AFTERPAYLOAD:"))
		case strings.HasPrefix(line, "MARKER:"):
			markerCreated, _ = strconv.ParseBool(strings.TrimPrefix(line, "MARKER:"))
		}
	}

	if len(kinds) == 0 {
		t.Errorf("expected completion candidates for %q, got none; full output:\n%s", "rancher get ", out)
	}
	if len(afterPayload) == 0 {
		t.Errorf("expected completion candidates after the payload line (completion must keep working, not just fail closed); full output:\n%s", out)
	}
	if markerCreated {
		t.Errorf("injection marker file was created: the completer executed attacker-controlled text instead of treating it as a literal argument; full output:\n%s", out)
	}
}

// TestPwshProtoV2RequestShape verifies that the v2 pwsh script constructs the
// correct argv vector: proto sentinel, words before cursor,
// --rancher-completion-cur=<word>, --generate-shell-completion.
func TestPwshProtoV2RequestShape(t *testing.T) {
	shell, ok := findPowerShell(t)
	if !ok {
		t.Skip("neither pwsh nor powershell found on PATH; skipping pwsh request-shape test")
	}
	binDir := buildRancherRecorder(t)
	scriptPath := filepath.Join(t.TempDir(), "pwsh.ps1")
	if err := os.WriteFile(scriptPath, pwshScript, 0o644); err != nil {
		t.Fatalf("writing pwsh.ps1: %v", err)
	}

	cases := []struct {
		name     string
		line     string   // input line for TabExpansion2
		cursor   int      // cursor column (-1 = end)
		wantArgs []string // expected argv after binary name
	}{
		{
			name:   "cursor-after-space empty cur",
			line:   "rancher get pod ",
			cursor: -1,
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get", "pod",
				"--rancher-completion-cur=",
				"--generate-shell-completion",
			},
		},
		{
			name:   "hostile cur with spaces (single-quoted)",
			line:   "rancher get 'a b c'",
			cursor: -1,
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get",
				"--rancher-completion-cur=a b c",
				"--generate-shell-completion",
			},
		},
		{
			name:   "ambiguity side A: flag-last partial cur",
			line:   "rancher get --cluster c-",
			cursor: -1,
			wantArgs: []string{
				"--rancher-completion-proto=2",
				"get", "--cluster",
				"--rancher-completion-cur=c-",
				"--generate-shell-completion",
			},
		},
		{
			name:   "ambiguity side B: flag+value consumed empty cur",
			line:   "rancher get --cluster c-test ",
			cursor: -1,
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
			argvFile := filepath.Join(t.TempDir(), "argv")
			cursorCol := tc.cursor
			if cursorCol < 0 {
				cursorCol = len(tc.line)
			}
			driver := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$env:PATH = %s + [System.IO.Path]::PathSeparator + $env:PATH
$env:RANCHER_ARGV_FILE = %s
. %s
$line = %s
$null = TabExpansion2 -inputScript $line -cursorColumn %d
`, psSingleQuote(binDir), psSingleQuote(argvFile), psSingleQuote(scriptPath), psSingleQuote(tc.line), cursorCol)

			driverPath := filepath.Join(t.TempDir(), "driver.ps1")
			if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
				t.Fatalf("writing driver.ps1: %v", err)
			}
			out, err := exec.Command(shell, "-NoProfile", "-NonInteractive", "-File", driverPath).CombinedOutput()
			if err != nil {
				t.Fatalf("pwsh driver: %v\n%s", err, out)
			}

			data, err := os.ReadFile(argvFile)
			if err != nil {
				t.Fatalf("argv file not written (recorder never invoked?): %v\nfull output:\n%s", err, out)
			}
			got := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
			if !slices.Equal(got, tc.wantArgs) {
				t.Errorf("argv mismatch\n got:  %q\n want: %q\nfull output:\n%s", got, tc.wantArgs, out)
			}
		})
	}
}

func splitNonEmpty(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// buildRancherRecorder builds a stand-in `rancher` that records the exact argv
// it receives (one element per line, to $RANCHER_ARGV_FILE) and prints a few
// candidates. It lets a test prove not just that a payload was not executed
// but that it reached the binary as one literal argument. Returns the dir to
// prepend to PATH.
func buildRancherRecorder(t *testing.T) string {
	t.Helper()
	src := "package main\n" +
		"import (\"fmt\"; \"os\"; \"strings\")\n" +
		"func main() {\n" +
		"  if f := os.Getenv(\"RANCHER_ARGV_FILE\"); f != \"\" {\n" +
		"    _ = os.WriteFile(f, []byte(strings.Join(os.Args[1:], \"\\n\")), 0o644)\n" +
		"  }\n" +
		"  fmt.Println(\"pod\"); fmt.Println(\"pods\"); fmt.Println(\"po\")\n" +
		"}\n"
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("writing recorder source: %v", err)
	}
	binDir := t.TempDir()
	binName := "rancher"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	build := exec.Command("go", "build", "-o", filepath.Join(binDir, binName), "main.go")
	build.Dir = srcDir
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GO111MODULE=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building rancher recorder: %s", out)
	}
	return binDir
}

// TestPwshCompletionDeliversPayloadAsLiteralArgument proves the hostile word
// reaches the binary as ONE literal argument, not merely that it goes
// unevaluated: a dropped or split token would also leave no marker. The
// payload is single-quoted and CONTAINS SPACES and a $(...) subexpression, and
// the recorder captures argv so the test asserts the EXACT argv vector and
// order - not just that some element matches. That catches whitespace
// splitting, a duplicated or retained token, or any other surrounding-argv
// corruption. The in-progress word "po" moves into the
// --rancher-completion-cur field, leaving exactly the v2 vector
// [--rancher-completion-proto=2, get, <payload>,
// --rancher-completion-cur=po, --generate-shell-completion].
func TestPwshCompletionDeliversPayloadAsLiteralArgument(t *testing.T) {
	shell, ok := findPowerShell(t)
	if !ok {
		t.Skip("neither pwsh nor powershell found on PATH")
	}
	binDir := buildRancherRecorder(t)
	scriptPath := filepath.Join(t.TempDir(), "pwsh.ps1")
	if err := os.WriteFile(scriptPath, pwshScript, 0o644); err != nil {
		t.Fatalf("writing pwsh.ps1: %v", err)
	}
	argvFile := filepath.Join(t.TempDir(), "argv")

	// A single-quoted PowerShell string is literal (no expansion); the
	// completer reads its .Value, so the binary receives the quote-stripped
	// content as one argument. Spaces + a $(...) make split-or-eval visible.
	payload := "a b $(Get-Date)"
	line := "rancher get '" + payload + "' po"
	want := []string{
		"--rancher-completion-proto=2",
		"get", payload,
		"--rancher-completion-cur=po",
		"--generate-shell-completion",
	}

	driver := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$env:PATH = %s + [System.IO.Path]::PathSeparator + $env:PATH
$env:RANCHER_ARGV_FILE = %s
. %s
$line = %s
$null = TabExpansion2 -inputScript $line -cursorColumn $line.Length
`, psSingleQuote(binDir), psSingleQuote(argvFile), psSingleQuote(scriptPath), psSingleQuote(line))

	driverPath := filepath.Join(t.TempDir(), "driver.ps1")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("writing driver.ps1: %v", err)
	}
	if out, err := exec.Command(shell, "-NoProfile", "-NonInteractive", "-File", driverPath).CombinedOutput(); err != nil {
		t.Fatalf("running pwsh driver: %v\noutput:\n%s", err, out)
	}

	recorded, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("recorder argv not written (binary never invoked as a literal command?): %v", err)
	}
	// The recorder joins argv with "\n"; the payload has no newline, so a
	// literal split on "\n" recovers exactly one element per argument.
	got := strings.Split(strings.TrimRight(string(recorded), "\n"), "\n")
	if !slices.Equal(got, want) {
		t.Errorf("argv vector mismatch: got %q, want %q (payload must arrive as one literal arg, po dropped, nothing else added)", got, want)
	}
}

// TestPwshCompletionRespectsCursorAndLiteralWildcards exercises two completion
// behaviors that had no execution coverage: cursor-boundary
// truncation (a token typed after the cursor must not affect completion) and
// literal-wildcard prefix matching (a '*' in the typed word must not act as a
// glob).
func TestPwshCompletionRespectsCursorAndLiteralWildcards(t *testing.T) {
	shell, ok := findPowerShell(t)
	if !ok {
		t.Skip("neither pwsh nor powershell found on PATH")
	}
	binDir := t.TempDir()
	binName := "rancher"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	build := exec.Command("go", "build", "-o", filepath.Join(binDir, binName), "github.com/rancher/rancher-cli/cmd/rancher")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building rancher: %s", out)
	}
	scriptPath := filepath.Join(t.TempDir(), "pwsh.ps1")
	if err := os.WriteFile(scriptPath, pwshScript, 0o644); err != nil {
		t.Fatalf("writing pwsh.ps1: %v", err)
	}

	// midline: cursor sits right after "pod" in "rancher get pod deployment";
	// the trailing "deployment" is past the cursor and must not corrupt the
	// completion of "pod". literal: "p*" must be matched as a literal prefix,
	// so no kind (pod/pods/...) is offered - a glob would match them all.
	driver := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$env:PATH = %s + [System.IO.Path]::PathSeparator + $env:PATH
. %s

$mid = 'rancher get pod deployment'
$cur = ('rancher get pod').Length
$rm = TabExpansion2 -inputScript $mid -cursorColumn $cur
Write-Output ('MIDLINE:' + (($rm.CompletionMatches | Where-Object { $_.ResultType -eq 'ParameterValue' } | ForEach-Object { $_.CompletionText }) -join ','))

$glob = 'rancher get p*'
$rg = TabExpansion2 -inputScript $glob -cursorColumn $glob.Length
Write-Output ('GLOB:' + (($rg.CompletionMatches | Where-Object { $_.ResultType -eq 'ParameterValue' } | ForEach-Object { $_.CompletionText }) -join ','))
`, psSingleQuote(binDir), psSingleQuote(scriptPath))

	driverPath := filepath.Join(t.TempDir(), "driver.ps1")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("writing driver.ps1: %v", err)
	}
	out, err := exec.Command(shell, "-NoProfile", "-NonInteractive", "-File", driverPath).CombinedOutput()
	if err != nil {
		t.Fatalf("running pwsh driver: %v\noutput:\n%s", err, out)
	}

	var midline, glob []string
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "MIDLINE:"):
			midline = splitNonEmpty(strings.TrimPrefix(line, "MIDLINE:"))
		case strings.HasPrefix(line, "GLOB:"):
			glob = splitNonEmpty(strings.TrimPrefix(line, "GLOB:"))
		}
	}

	hasPod := false
	for _, m := range midline {
		if m == "pod" {
			hasPod = true
		}
	}
	if !hasPod {
		t.Errorf("mid-line completion of \"pod\" did not offer \"pod\"; a token past the cursor likely leaked into the request. got %q; full output:\n%s", midline, out)
	}
	if len(glob) != 0 {
		t.Errorf("literal-wildcard prefix \"p*\" matched %q; the * was treated as a glob instead of a literal. full output:\n%s", glob, out)
	}
}
