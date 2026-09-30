package completions

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPwshCompletesFilesForAnExecCommandPath asserts PowerShell answers
// rancher kubeconfig --exec-command ./ with files from the working
// directory: the binary returns nothing for a path word, and PowerShell
// falls back to its own path completion. Skips without PowerShell.
func TestPwshCompletesFilesForAnExecCommandPath(t *testing.T) {
	shell, ok := findPowerShell(t)
	if !ok {
		t.Skip("neither pwsh nor powershell found on PATH")
	}
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
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "myrancher"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	driver := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$env:PATH = %s + [System.IO.Path]::PathSeparator + $env:PATH
$env:RANCHER_CLI_CONFIG_DIR = %s
$env:RANCHER_CLI_CREDENTIAL_STORE = 'file'
Set-Location %s
. %s
$line = 'rancher kubeconfig --cluster c-1 --exec-command ./'
$r = TabExpansion2 -inputScript $line -cursorColumn $line.Length
Write-Output ('MATCHES:' + (($r.CompletionMatches | ForEach-Object { $_.CompletionText }) -join ','))
`, psSingleQuote(binDir), psSingleQuote(t.TempDir()), psSingleQuote(work), psSingleQuote(scriptPath))
	driverPath := filepath.Join(t.TempDir(), "driver.ps1")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), shell, "-NoProfile", "-NonInteractive", "-File", driverPath).CombinedOutput()
	if err != nil {
		t.Fatalf("running pwsh driver: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "myrancher") {
		t.Errorf("PowerShell did not complete the file for a path word:\n%s", out)
	}
}
