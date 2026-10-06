//go:build !windows

package completions

// Deliberately plain-stdlib (no testify): these tests execute under real
// external shells and are kept dependency-light on purpose.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

var (
	rancherBinOnce sync.Once
	rancherBinDir  string
	rancherBinErr  error
)

// realRancher builds the rancher binary once for the package's tests and
// returns the directory that holds it. TestMain removes the directory after
// the run.
func realRancher() (string, error) {
	rancherBinOnce.Do(func() {
		rancherBinDir, rancherBinErr = os.MkdirTemp("", "rancher-bin-")
		if rancherBinErr != nil {
			return
		}
		build := exec.Command("go", "build", "-o", filepath.Join(rancherBinDir, "rancher"), "github.com/rancher/rancher-cli/cmd/rancher")
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			rancherBinErr = fmt.Errorf("building rancher: %v\n%s", err, out)
		}
	})
	return rancherBinDir, rancherBinErr
}

func TestMain(m *testing.M) {
	code := m.Run()
	if rancherBinDir != "" {
		_ = os.RemoveAll(rancherBinDir)
	}
	os.Exit(code)
}

// tabSession is a hermetic place to press TAB in: the real rancher binary on
// PATH, a config directory and a working directory that start empty, and a
// server the environment names that counts the requests it receives.
type tabSession struct {
	cfgDir   string
	workDir  string
	requests atomic.Int32
	env      []string
}

func newTabSession(t *testing.T) *tabSession {
	t.Helper()
	binDir, err := realRancher()
	if err != nil {
		t.Fatal(err)
	}
	s := &tabSession{cfgDir: t.TempDir(), workDir: t.TempDir()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	s.env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+t.TempDir(),
		"RANCHER_CLI_CONFIG_DIR="+s.cfgDir,
		"RANCHER_CLI_CREDENTIAL_STORE=file",
		"RANCHER_CLI_URL="+srv.URL,
		"RANCHER_CLI_TOKEN=token-abc:secret",
	)
	return s
}

// run runs a shell on the driver in the session and returns its output.
func (s *tabSession) run(t *testing.T, shell string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), shell, args...)
	cmd.Dir = s.workDir
	cmd.Env = s.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", shell, err, out)
	}
	return string(out)
}

// listDir returns the paths under dir.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err == nil && p != dir {
			paths = append(paths, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

// candidatesOf returns the candidate lines a driver printed, each prefixed
// "C:".
func candidatesOf(out string) []string {
	var got []string
	for line := range strings.SplitSeq(out, "\n") {
		if c, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "C:"); ok {
			got = append(got, strings.TrimSpace(c))
		}
	}
	return got
}

// tabLine is a command line with the cursor at its end: the words typed
// before the cursor and the word being completed.
type tabLine struct {
	words   []string
	current string
}

// shellOrSkip returns the path of the shell, or skips the test when the
// machine has none. It runs before anything is built.
func shellOrSkip(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not found on PATH", name)
	}
	return path
}

// tabAfterDoubleDash presses TAB on the lines a completion script must
// handle after a "--", through tabs, the shell driver for the script under
// test, which returns the candidates for each line in order. A line that would
// write a file, or send a request, if the command ran must leave the config
// directory and the working directory empty and send nothing, and must offer
// no flag, even when the word being completed matches one; a line that
// completes an argument must offer it.
func tabAfterDoubleDash(t *testing.T, tabs func(t *testing.T, s *tabSession, lines []tabLine) [][]string) {
	t.Helper()
	s := newTabSession(t)
	lines := []tabLine{
		{words: []string{"rancher", "config", "add-server", "--url", "https://example.invalid", "--apply", "--"}, current: "--ur"},
		{words: []string{"rancher", "config", "add-server", "--url", "https://example.invalid", "--apply", "--", "myserver"}},
		{words: []string{"rancher", "config", "add-server", "--url", "https://example.invalid", "--apply", "--"}, current: "my"},
		{words: []string{"rancher", "delete", "project", "--cluster", "c1", "--apply", "--yes", "--", "p1"}},
		{words: []string{"rancher", "get", "pod", "--cluster", "c1", "--", "mypod"}},
		{words: []string{"rancher", "get", "pod", "--cluster", "c1", "--", "mypod"}, current: "--c"},
		{words: []string{"rancher", "get", "pod", "--cluster", "c1", "--"}, current: "--c"},
	}
	offering := tabLine{words: []string{"rancher", "completion", "--"}, current: "z"}
	got := tabs(t, s, append(slices.Clone(lines), offering))
	if len(got) != len(lines)+1 {
		t.Fatalf("the driver returned %d candidate lists for %d lines", len(got), len(lines)+1)
	}
	for i, l := range lines {
		for _, c := range got[i] {
			if strings.HasPrefix(c, "-") {
				t.Errorf("%v %q: offered the flag %q after --", l.words, l.current, c)
			}
		}
	}
	if n := s.requests.Load(); n != 0 {
		t.Errorf("%d requests reached the server, want none", n)
	}
	if files := listDir(t, s.cfgDir); len(files) != 0 {
		t.Errorf("files appeared in the config directory: %v", files)
	}
	if files := listDir(t, s.workDir); len(files) != 0 {
		t.Errorf("files appeared in the working directory: %v", files)
	}
	if want := []string{"zsh"}; !slices.Equal(got[len(lines)], want) {
		t.Errorf("rancher completion -- z: offered %q, want %q", got[len(lines)], want)
	}
}

// eachLine adapts a driver that presses TAB on one line to the batch form.
func eachLine(tab func(t *testing.T, s *tabSession, l tabLine) []string) func(t *testing.T, s *tabSession, lines []tabLine) [][]string {
	return func(t *testing.T, s *tabSession, lines []tabLine) [][]string {
		t.Helper()
		var all [][]string
		for _, l := range lines {
			all = append(all, tab(t, s, l))
		}
		return all
	}
}

func bashTab(bash string) func(t *testing.T, s *tabSession, l tabLine) []string {
	return func(t *testing.T, s *tabSession, l tabLine) []string {
		t.Helper()
		scriptPath := filepath.Join(t.TempDir(), "bash.sh")
		if err := os.WriteFile(scriptPath, bashScript, 0o644); err != nil {
			t.Fatal(err)
		}
		words := append(slices.Clone(l.words), l.current)
		quoted := make([]string, len(words))
		for i, w := range words {
			quoted[i] = "'" + strings.ReplaceAll(w, "'", "'\\''") + "'"
		}
		driver := fmt.Sprintf(`
set +e
_get_comp_words_by_ref() { words=("${COMP_WORDS[@]}"); cword="${COMP_CWORD}"; cur="${words[$cword]}"; }
source %q
COMP_WORDS=(%s)
COMP_CWORD=%d
COMP_TYPE=""
__rancher_bash_autocomplete
for c in "${COMPREPLY[@]}"; do printf 'C:%%s\n' "$c"; done
`, scriptPath, strings.Join(quoted, " "), len(words)-1)
		return candidatesOf(s.run(t, bash, "-c", driver))
	}
}

func zshTab(zsh string) func(t *testing.T, s *tabSession, l tabLine) []string {
	return func(t *testing.T, s *tabSession, l tabLine) []string {
		t.Helper()
		scriptPath := filepath.Join(t.TempDir(), "zsh.sh")
		if err := os.WriteFile(scriptPath, zshScript, 0o644); err != nil {
			t.Fatal(err)
		}
		words := append(slices.Clone(l.words), l.current)
		quoted := make([]string, len(words))
		for i, w := range words {
			quoted[i] = "'" + strings.ReplaceAll(w, "'", "'\\''") + "'"
		}
		driver := fmt.Sprintf(`
_describe() { local c; for c in "${(@P)2}"; do print -r -- "C:${c%%%%:*}"; done }
_files() { : }
autoload -Uz compinit 2>/dev/null || true
source %q
words=(%s)
_rancher
`, scriptPath, strings.Join(quoted, " "))
		return candidatesOf(s.run(t, zsh, "-c", driver))
	}
}

func fishTab(fish string) func(t *testing.T, s *tabSession, l tabLine) []string {
	return func(t *testing.T, s *tabSession, l tabLine) []string {
		t.Helper()
		scriptPath := filepath.Join(t.TempDir(), "fish.sh")
		if err := os.WriteFile(scriptPath, fishScript, 0o644); err != nil {
			t.Fatal(err)
		}
		var opcLines strings.Builder
		for _, w := range l.words {
			fmt.Fprintf(&opcLines, "            printf '%%s\\n' '%s'\n", strings.ReplaceAll(w, "'", "\\'"))
		}
		driver := fmt.Sprintf(`
function commandline
    switch $argv[1]
        case -opc
%s        case -ct
            echo '%s'
    end
end
source %q
for line in (__rancher_perform_completion)
    printf 'C:%%s\n' (string split -m 1 \t -- $line)[1]
end
`, opcLines.String(), strings.ReplaceAll(l.current, "'", "\\'"), scriptPath)
		return candidatesOf(s.run(t, fish, "-c", driver))
	}
}

// pwshTabs presses TAB on every line in one PowerShell process: the driver
// prints "L:<n>" before the candidates of line n.
func pwshTabs(shell string) func(t *testing.T, s *tabSession, lines []tabLine) [][]string {
	return func(t *testing.T, s *tabSession, lines []tabLine) [][]string {
		t.Helper()
		scriptPath := filepath.Join(t.TempDir(), "pwsh.ps1")
		if err := os.WriteFile(scriptPath, pwshScript, 0o644); err != nil {
			t.Fatal(err)
		}
		quoted := make([]string, len(lines))
		for i, l := range lines {
			quoted[i] = psSingleQuote(strings.Join(l.words, " ") + " " + l.current)
		}
		driver := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
. %s
$lines = @(%s)
for ($i = 0; $i -lt $lines.Count; $i++) {
    Write-Output ('L:' + $i)
    $line = $lines[$i]
    $r = TabExpansion2 -inputScript $line -cursorColumn $line.Length
    foreach ($m in $r.CompletionMatches) { Write-Output ('C:' + $m.CompletionText) }
}
`, psSingleQuote(scriptPath), strings.Join(quoted, ", "))
		driverPath := filepath.Join(t.TempDir(), "driver.ps1")
		if err := os.WriteFile(driverPath, []byte(driver), 0o644); err != nil {
			t.Fatal(err)
		}
		out := s.run(t, shell, "-NoProfile", "-NonInteractive", "-File", driverPath)
		all := make([][]string, len(lines))
		current := -1
		for line := range strings.SplitSeq(out, "\n") {
			line = strings.TrimRight(line, "\r")
			if n, ok := strings.CutPrefix(line, "L:"); ok {
				current, _ = strconv.Atoi(n)
			} else if c, ok := strings.CutPrefix(line, "C:"); ok && current >= 0 {
				all[current] = append(all[current], strings.TrimSpace(c))
			}
		}
		return all
	}
}

// TestBashTabAfterDoubleDashRunsNothing drives the bash script, with the real
// binary, on lines typed after a "--": completion never runs the command.
func TestBashTabAfterDoubleDashRunsNothing(t *testing.T) {
	t.Parallel()
	bash := shellOrSkip(t, "bash")
	tabAfterDoubleDash(t, eachLine(bashTab(bash)))
}

// TestZshTabAfterDoubleDashRunsNothing is the zsh counterpart of
// TestBashTabAfterDoubleDashRunsNothing.
func TestZshTabAfterDoubleDashRunsNothing(t *testing.T) {
	t.Parallel()
	zsh := shellOrSkip(t, "zsh")
	tabAfterDoubleDash(t, eachLine(zshTab(zsh)))
}

// TestFishTabAfterDoubleDashRunsNothing is the fish counterpart of
// TestBashTabAfterDoubleDashRunsNothing.
func TestFishTabAfterDoubleDashRunsNothing(t *testing.T) {
	t.Parallel()
	fish := shellOrSkip(t, "fish")
	tabAfterDoubleDash(t, eachLine(fishTab(fish)))
}

// TestPwshTabAfterDoubleDashRunsNothing is the PowerShell counterpart of
// TestBashTabAfterDoubleDashRunsNothing.
func TestPwshTabAfterDoubleDashRunsNothing(t *testing.T) {
	t.Parallel()
	shell, ok := findPowerShell(t)
	if !ok {
		t.Skip("neither pwsh nor powershell found on PATH")
	}
	tabAfterDoubleDash(t, pwshTabs(shell))
}
