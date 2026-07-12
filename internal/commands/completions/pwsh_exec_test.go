package completions

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
// where no PowerShell is installed (the CI gate for this repo runs on
// Linux without one).
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

	// internal/commands/completions -> internal/commands -> internal -> module root.
	build := exec.Command("go", "build", "-o", binPath, "../../..")
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
	for _, line := range strings.Split(string(out), "\n") {
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

func splitNonEmpty(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
