package completions

// Deliberately plain-stdlib (no testify): these tests execute under a real
// external PowerShell and are kept dependency-light on purpose.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildRancherLineStub builds a stand-in `rancher` that prints the lines
// of the file named by $RANCHER_STUB_FILE, so a test chooses exactly what
// the completer receives. Returns the dir to prepend to PATH.
func buildRancherLineStub(t *testing.T) string {
	t.Helper()
	src := "package main\n" +
		"import (\"os\")\n" +
		"func main() {\n" +
		"  data, err := os.ReadFile(os.Getenv(\"RANCHER_STUB_FILE\"))\n" +
		"  if err != nil {\n    os.Exit(1)\n  }\n" +
		"  os.Stdout.Write(data)\n" +
		"}\n"
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("writing stub source: %v", err)
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
		t.Fatalf("building rancher stub: %s", out)
	}
	return binDir
}

// TestPwshShowsTheDescriptionAsToolTip asserts the script splits a
// candidate line at its first colon: the token is the completion text and
// the text after it is the ToolTip, a colon inside a description is kept,
// and a bare token has itself as the tooltip (PowerShell refuses an empty
// one).
func TestPwshShowsTheDescriptionAsToolTip(t *testing.T) {
	shell := requireShell(t, "pwsh", "powershell")
	binDir := buildRancherLineStub(t)
	scriptPath := filepath.Join(t.TempDir(), "pwsh.ps1")
	if err := os.WriteFile(scriptPath, pwshScript, 0o644); err != nil {
		t.Fatalf("writing pwsh.ps1: %v", err)
	}
	linesFile := filepath.Join(t.TempDir(), "lines")
	lines := "u-abc12:alice (Alice Smith)\nu-col01:dev: build team\nu-plain\nproject:Project namespace commands (managed with `rancher grant`)\n"
	if err := os.WriteFile(linesFile, []byte(lines), 0o644); err != nil {
		t.Fatalf("writing stub lines: %v", err)
	}

	driver := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$env:PATH = %s + [System.IO.Path]::PathSeparator + $env:PATH
$env:RANCHER_STUB_FILE = %s
. %s
$line = 'rancher get user '
$r = TabExpansion2 -inputScript $line -cursorColumn $line.Length
foreach ($m in $r.CompletionMatches) { Write-Output ('MATCH:' + $m.CompletionText + '|' + $m.ToolTip) }
`, psSingleQuote(binDir), psSingleQuote(linesFile), psSingleQuote(scriptPath))
	driverPath := filepath.Join(t.TempDir(), "driver.ps1")
	if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
		t.Fatalf("writing driver.ps1: %v", err)
	}
	out, err := exec.Command(shell, "-NoProfile", "-NonInteractive", "-File", driverPath).CombinedOutput()
	if err != nil {
		t.Fatalf("pwsh driver: %v\n%s", err, out)
	}

	var got []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if m, ok := strings.CutPrefix(strings.TrimSpace(line), "MATCH:"); ok {
			got = append(got, m)
		}
	}
	want := []string{
		"u-abc12|alice (Alice Smith)",
		"u-col01|dev: build team",
		"u-plain|u-plain",
		"project|Project namespace commands (managed with `rancher grant`)",
	}
	if len(got) != len(want) {
		t.Fatalf("matches\n got:  %q\n want: %q\nfull output:\n%s", got, want, out)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("match %d\n got:  %q\n want: %q", i, got[i], want[i])
		}
	}
}
