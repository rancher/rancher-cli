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
	"strings"
	"testing"
)

// TestPwshKeyValueCandidatesCarryNoTrailingSpace asserts the PowerShell side
// of the continuation handling for key= candidates (create project's
// --container-default-resource-limit): PowerShell inserts CompletionText
// verbatim, so each candidate must reach it ending in "=" with no trailing
// space, in the space and the equals form, through the real binary and the
// emitted pwsh.ps1. bash and zsh have their own boundary tests; fish
// withholds the space itself for a completion ending in "=" or ",".
func TestPwshKeyValueCandidatesCarryNoTrailingSpace(t *testing.T) {
	shell := requireShell(t, "pwsh", "powershell")
	binDir := t.TempDir()
	binName := "rancher"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(binDir, binName), "github.com/rancher/rancher-cli/cmd/rancher")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building rancher: %s", out)
	}
	scriptPath := filepath.Join(t.TempDir(), "pwsh.ps1")
	if err := os.WriteFile(scriptPath, pwshScript, 0o644); err != nil {
		t.Fatalf("writing pwsh.ps1: %v", err)
	}

	driver := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$env:PATH = %s + [System.IO.Path]::PathSeparator + $env:PATH
$env:RANCHER_CLI_CONFIG_DIR = %s
$env:RANCHER_CLI_CREDENTIAL_STORE = 'file'
. %s

$space = 'rancher create project web --container-default-resource-limit lim'
$rs = TabExpansion2 -inputScript $space -cursorColumn $space.Length
Write-Output ('SPACE:' + (($rs.CompletionMatches | ForEach-Object { '[' + $_.CompletionText + ']' }) -join ','))

$equals = 'rancher create project web --container-default-resource-limit=limitsM'
$re = TabExpansion2 -inputScript $equals -cursorColumn $equals.Length
Write-Output ('EQUALS:' + (($re.CompletionMatches | ForEach-Object { '[' + $_.CompletionText + ']' }) -join ','))
`, psSingleQuote(binDir), psSingleQuote(t.TempDir()), psSingleQuote(scriptPath))

	driverPath := filepath.Join(t.TempDir(), "driver.ps1")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("writing driver.ps1: %v", err)
	}
	out, err := exec.CommandContext(t.Context(), shell, "-NoProfile", "-NonInteractive", "-File", driverPath).CombinedOutput()
	if err != nil {
		t.Fatalf("running pwsh driver: %v\noutput:\n%s", err, out)
	}

	got := map[string][]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		for _, key := range []string{"SPACE", "EQUALS"} {
			if rest, ok := strings.CutPrefix(line, key+":"); ok {
				got[key] = splitNonEmpty(rest)
			}
		}
	}
	want := map[string][]string{
		"SPACE":  {"[limitsCpu=]", "[limitsMemory=]"},
		"EQUALS": {"[--container-default-resource-limit=limitsMemory=]"},
	}
	for key, w := range want {
		if !slices.Equal(got[key], w) {
			t.Errorf("%s: CompletionText %q, want %q (bracketed, so a trailing space would show); full output:\n%s", key, got[key], w, out)
		}
	}
}
