package completions

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testfs"
)

// TestPwshCompletesFilesForAnExecCommandPath asserts PowerShell answers
// rancher kubeconfig --exec-command ./ with files from the working
// directory: the binary returns nothing for a path word, and PowerShell
// falls back to its own path completion. Skips without PowerShell
// (fails instead when CI is set).
func TestPwshCompletesFilesForAnExecCommandPath(t *testing.T) {
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
		t.Fatal(err)
	}
	work := t.TempDir()
	testfs.WriteExecutable(t, filepath.Join(work, "myrancher"), []byte("x"))
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
