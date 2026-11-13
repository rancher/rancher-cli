package conformance

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/testfs"
)

// pasteHelperEnv makes a copy of the test binary print its arguments as json
// and exit, so a printed command pasted into a shell shows what the shell
// handed to the program.
const pasteHelperEnv = "EXACTVALUE_PASTE_HELPER"

// pasteHelper prints the arguments of this process as one line of json with
// every non-ASCII rune as a \u escape, so a console code page cannot change
// what the test reads back.
func pasteHelper() {
	data, err := json.Marshal(os.Args[1:])
	if err != nil {
		os.Exit(2)
	}
	var sb strings.Builder
	for _, r := range string(data) {
		switch {
		case r < 0x80:
			sb.WriteRune(r)
		case r < 0x10000:
			fmt.Fprintf(&sb, `\u%04x`, r)
		default:
			r -= 0x10000
			fmt.Fprintf(&sb, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		}
	}
	sb.WriteString("\n")
	_, _ = io.WriteString(os.Stdout, sb.String())
	os.Exit(0)
}

// pasteShell is one shell the tests paste a printed command into: the program
// that runs a script file, the script's file name suffix and the text it
// starts with.
type pasteShell struct {
	program string
	args    func(script string) []string
	suffix  string
	header  string
}

// pasteRun is one run of a script: the arguments the program received, and
// the error of the run (a parse error of the shell or a failed program).
type pasteRun struct {
	args []string
	err  error
}

// bom starts a PowerShell script so Windows PowerShell 5.1 reads it as UTF-8.
const bom = "\ufeff"

// newPasteDir copies the running test binary into a new directory as the
// program rancher, which the helper mode answers, and returns the directory.
func newPasteDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	name := "rancher"
	if testfs.OnWindows() {
		name = "rancher.exe"
	}
	self, err := os.Executable()
	require.NoError(t, err)
	data, err := os.ReadFile(self)
	require.NoError(t, err)
	testfs.WriteExecutable(t, filepath.Join(dir, name), data)
	return dir
}

// runPaste writes line to a script file of the shell in dir, runs it with dir
// first on PATH and the helper mode on, and returns what the program received.
// Every process is started with exec.CommandContext and waited for before it
// returns, so the copy of the program can be removed when the test ends.
func runPaste(t *testing.T, sh pasteShell, dir, line string) pasteRun {
	t.Helper()
	script := filepath.Join(dir, "line"+sh.suffix)
	require.NoError(t, os.WriteFile(script, []byte(sh.header+line+"\n"), 0o600))
	cmd := exec.CommandContext(t.Context(), sh.program, sh.args(script)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		pasteHelperEnv+"=1")
	out, err := cmd.Output()
	run := pasteRun{err: err}
	if text := strings.TrimSpace(string(out)); text != "" {
		var args []string
		if jerr := json.Unmarshal([]byte(text), &args); jerr == nil {
			run.args = args
		}
	}
	return run
}

// pasteShellOrSkip returns the program a shell lookup found, or ends the test
// for a missing shell: it skips locally and fails when the CI environment
// variable is set, because a skipped shell test asserts nothing about that
// shell.
func pasteShellOrSkip(t *testing.T, name, program string, lookErr error) string {
	t.Helper()
	if lookErr != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("CI is set, so a missing shell fails instead of skipping: %s not found on PATH", name)
		}
		t.Skipf("%s not found on PATH", name)
	}
	t.Logf("shell %s: %s", name, program)
	return program
}

func posixShell(program string) pasteShell {
	return pasteShell{program: program, args: func(script string) []string { return []string{script} }}
}

func powerShell(program, header string) pasteShell {
	return pasteShell{
		program: program, suffix: ".ps1", header: bom + header,
		args: func(script string) []string {
			return []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script}
		},
	}
}

// legacyArguments is the line that makes PowerShell 7 pass arguments to a
// program the way Windows PowerShell 5.1 always does.
const legacyArguments = "$PSNativeCommandArgumentPassing = 'Legacy'\n"

// pasteRow is a value the builder prints, and the arguments a command built
// from it must reach a program with.
type pasteRow struct {
	name  string
	value string
}

// acceptedPasteRows are the values the rule accepts. The injection row holds
// text each shell family would run if it read the quoting differently.
func acceptedPasteRows(injection string) []pasteRow {
	return []pasteRow{
		{"plain", "plain"},
		{"command substitution", "$(echo hi)"},
		{"backtick", "`id`"},
		{"space", "a b"},
		{"leading dash", "-x"},
		{"leading dash and dot", "-a.b"},
		{"windows path", `C:\Users\a b\rancher`},
		{"u umlaut", "\u00fc"},
		{"left double quote", "\u201c"},
		{"right double quote", "\u201d"},
		{"low double quote", "\u201e"},
		{"punctuation", "a;b|c&d<e>f!g#h"},
		{"inner backslash", `a\b`},
		{"double dash", "--"},
		{"injection", injection},
	}
}

// refusedPasteValues are the values the rule refuses, each with the shell
// that motivates it.
var refusedPasteValues = []pasteRow{
	{"apostrophe", "o'b"},
	{"left single quote", "o\u2018b"},
	{"right single quote", "o\u2019b"},
	{"low single quote", "o\u201ab"},
	{"reversed single quote", "o\u201bb"},
	{"double quote", `a"b`},
	{"empty", ""},
	{"final backslash", `a b\`},
	{"two backslashes", `\\server\share`},
	{"hidden rune", "abc\u200b"},
	{"line break", "a\nb"},
}

// pasteLine is the line a builder prints for a command with the value, and
// whether the builder prints it.
func pasteLine(value string) (string, bool) {
	c := clierr.FileCommand("get users").Arg(value)
	return c.String(), c.OK()
}

// expectedArgs is what a program must receive from the command of pasteLine.
func expectedArgs(value string) []string {
	if strings.HasPrefix(strings.TrimSpace(value), "-") {
		return []string{"get", "users", "--", value}
	}
	return []string{"get", "users", value}
}

// pasteDirectory is a Windows directory with a space, written after --config
// by FileCommandIn.
const pasteDirectory = `C:\Users\a b\rancher`

// pasteForm is one way the builders print a value: the line for a value, and
// the arguments a program must receive from it. refused is the line of a
// value the builder leaves out.
type pasteForm struct {
	name    string
	line    func(value string) (string, bool)
	args    func(value string) []string
	refused string
}

// typedLine is the line a TypedLine prints, "" when it leaves the line out.
func typedLine(l clierr.TypedLine) (string, bool) { return l.Field(), l.OK() }

// pasteForms are every builder form that prints a value: a positional
// argument, a flag value, a value joined to its flag, a flag value that joins
// a bare word to a quoted one, a bare word through ShellWord and through
// TypedLine.Bare, and a command that starts with a --config directory.
var pasteForms = []pasteForm{
	{"argument", pasteLine, expectedArgs, clierr.LeftOutNote},
	{"flag value",
		func(v string) (string, bool) {
			c := clierr.FileCommand("get users").FlagValue("--flag", v)
			return c.String(), c.OK()
		},
		func(v string) []string { return []string{"get", "users", "--flag", v} },
		clierr.LeftOutNote},
	{"joined value",
		func(v string) (string, bool) {
			return typedLine(clierr.NewTypedLine().Bare("get").Bare("users").Joined("--flag", v))
		},
		func(v string) []string { return []string{"get", "users", "--flag=" + v} },
		""},
	{"bare word joined to a quoted one",
		func(v string) (string, bool) {
			frag, ok := clierr.FlagFragmentAfterWord("--name", "local-", v)
			return "rancher get users " + frag, ok
		},
		func(v string) []string { return []string{"get", "users", "--name", "local-" + v} },
		"rancher get users "},
	{"shell word",
		func(v string) (string, bool) {
			w, ok := clierr.ShellWord(v)
			return "rancher get users " + w, ok
		},
		func(v string) []string { return []string{"get", "users", v} },
		"rancher get users "},
	{"typed bare word",
		func(v string) (string, bool) {
			return typedLine(clierr.NewTypedLine().Bare("get").Bare("users").Bare(v))
		},
		func(v string) []string { return []string{"get", "users", v} },
		""},
	{"config directory",
		func(v string) (string, bool) {
			c := clierr.FileCommandIn(pasteDirectory, "get users").Arg(v)
			return c.String(), c.OK()
		},
		func(v string) []string {
			return append([]string{"--config", pasteDirectory}, expectedArgs(v)...)
		},
		clierr.LeftOutNote},
}

// assertAcceptedRows asserts every accepted value reaches the program exactly
// and the injection never runs.
func assertAcceptedRows(t *testing.T, sh pasteShell, dir, sentinel, injection string) {
	t.Helper()
	for _, row := range acceptedPasteRows(injection) {
		for _, form := range pasteForms {
			name := form.name + ": " + row.name
			line, ok := form.line(row.value)
			require.True(t, ok, name)
			run := runPaste(t, sh, dir, line)
			require.NoError(t, run.err, name)
			assert.Equal(t, form.args(row.value), run.args, "%s: %s", name, line)
			assert.NoFileExists(t, sentinel, name)
		}
	}
}

// assertRefusedRows asserts every builder form leaves out a command for each
// refused value, and a command builder says why.
func assertRefusedRows(t *testing.T) {
	t.Helper()
	for _, row := range refusedPasteValues {
		for _, form := range pasteForms {
			name := form.name + ": " + row.name
			line, ok := form.line(row.value)
			assert.False(t, ok, name)
			assert.Equal(t, form.refused, line, name)
		}
	}
	for _, dir := range []string{"o'b", `a\\b`, `a b\`, "a\u200b"} {
		c := clierr.FileCommandIn(dir, "get users").Arg("plain")
		assert.False(t, c.OK(), dir)
		assert.Equal(t, clierr.LeftOutNote, c.String(), dir)
	}
}

// assertTodaysFormArrivesExactly asserts that the single-quote form the CLI
// printed before arrives unchanged in a shell of the POSIX family, which is
// why the refusal is for the other shells.
func assertTodaysFormArrivesExactly(t *testing.T, sh pasteShell, dir string) {
	t.Helper()
	for _, row := range refusedPasteValues {
		if row.name == "hidden rune" || row.name == "line break" {
			continue
		}
		line := "rancher get users " + clierr.ShellQuote(row.value)
		run := runPaste(t, sh, dir, line)
		require.NoError(t, run.err, row.name)
		assert.Equal(t, []string{"get", "users", row.value}, run.args, "%s: %s", row.name, line)
	}
}

func posixInjection(sentinel string) string { return "$(touch " + sentinel + ")" }

func powerShellInjection(sentinel string) string {
	return "$(New-Item -Path " + filepath.ToSlash(filepath.Dir(sentinel)) + " -Name " + filepath.Base(sentinel) + ")"
}

func testPosixPaste(t *testing.T, sh pasteShell) {
	t.Helper()
	dir := newPasteDir(t)
	sentinel := filepath.Join(dir, "sentinel")
	assertAcceptedRows(t, sh, dir, sentinel, posixInjection(sentinel))
	assertRefusedRows(t)
	assertTodaysFormArrivesExactly(t, sh, dir)
	assert.NoFileExists(t, sentinel)
}

// TestExactValuePasteSh asserts a command the builder prints reaches a
// program with exactly the values it was built from under POSIX sh, and the
// form the CLI printed before also does.
func TestExactValuePasteSh(t *testing.T) {
	t.Parallel()
	program, err := exec.LookPath("sh")
	testPosixPaste(t, posixShell(pasteShellOrSkip(t, "sh", program, err)))
}

// TestExactValuePasteBash asserts the same under bash.
func TestExactValuePasteBash(t *testing.T) {
	t.Parallel()
	program, err := exec.LookPath("bash")
	testPosixPaste(t, posixShell(pasteShellOrSkip(t, "bash", program, err)))
}

// TestExactValuePasteZsh asserts the same under zsh.
func TestExactValuePasteZsh(t *testing.T) {
	t.Parallel()
	program, err := exec.LookPath("zsh")
	testPosixPaste(t, posixShell(pasteShellOrSkip(t, "zsh", program, err)))
}

// TestExactValuePasteFish asserts a command the builder prints reaches a
// program with exactly the values it was built from under fish, and that the
// form the CLI printed before did not for a final backslash (fish reads the
// closing quote as escaped) or two backslashes in a row (fish reads them as
// one).
func TestExactValuePasteFish(t *testing.T) {
	t.Parallel()
	program, err := exec.LookPath("fish")
	sh := posixShell(pasteShellOrSkip(t, "fish", program, err))
	dir := newPasteDir(t)
	sentinel := filepath.Join(dir, "sentinel")
	assertAcceptedRows(t, sh, dir, sentinel, posixInjection(sentinel))
	assertRefusedRows(t)

	final := runPaste(t, sh, dir, "rancher get users "+clierr.ShellQuote(`a b\`))
	assert.True(t, final.err != nil || len(final.args) != 3 || final.args[2] != `a b\`, "a final backslash arrives as written: %v", final.args)
	two := runPaste(t, sh, dir, "rancher get users "+clierr.ShellQuote(`\\server\share`))
	require.NoError(t, two.err)
	assert.Equal(t, []string{"get", "users", `\server\share`}, two.args, "fish reads two backslashes in single quotes as one")
	assert.NoFileExists(t, sentinel)
}

func testPwshPaste(t *testing.T, program string) {
	t.Helper()
	sh := powerShell(program, "")
	legacy := powerShell(program, legacyArguments)
	dir := newPasteDir(t)
	sentinel := filepath.Join(dir, "sentinel")
	assertAcceptedRows(t, sh, dir, sentinel, powerShellInjection(sentinel))
	assertRefusedRows(t)

	for _, name := range []string{"left single quote", "right single quote", "low single quote", "reversed single quote"} {
		var value string
		for _, row := range refusedPasteValues {
			if row.name == name {
				value = row.value
			}
		}
		run := runPaste(t, sh, dir, "rancher get users "+clierr.ShellQuote(value))
		require.Error(t, run.err, "%s: a curly single quote ends a single-quoted string", name)
	}

	hostile := func(sentinel string) string { return "o'b" + powerShellInjection(sentinel) + "'c" }
	evidence := filepath.Join(dir, "evidence")
	runPaste(t, sh, dir, "rancher get users "+clierr.ShellQuote(hostile(evidence)))
	assert.FileExists(t, evidence, "the form the CLI printed before runs the text between two apostrophes")
	blocked := filepath.Join(dir, "blocked")
	if line, ok := pasteLine(hostile(blocked)); ok {
		runPaste(t, sh, dir, line)
	} else {
		assert.Equal(t, clierr.LeftOutNote, line)
	}
	assert.NoFileExists(t, blocked, "a value with an apostrophe never reaches the shell")
	assert.NoFileExists(t, sentinel)

	for _, value := range []string{`a" --yes`, ""} {
		changed := runPaste(t, legacy, dir, "rancher get users "+clierr.ShellQuote(value)+" --dry-run")
		require.NoError(t, changed.err, "legacy argument passing runs the line")
		assert.NotEqual(t, []string{"get", "users", value, "--dry-run"}, changed.args, "legacy argument passing changes %q", value)
	}
}

// TestExactValuePastePwsh asserts a command the builder prints reaches a
// program with exactly the values it was built from under PowerShell 7, that
// a value after an apostrophe in the form the CLI printed before runs as
// code, that a curly single quote is a parse error there, and that the legacy
// argument passing of Windows PowerShell 5.1 changes a double quote and
// drops an empty value.
func TestExactValuePastePwsh(t *testing.T) {
	t.Parallel()
	program, err := exec.LookPath("pwsh")
	testPwshPaste(t, pasteShellOrSkip(t, "pwsh", program, err))
}

// TestExactValuePastePowerShell51 asserts the same checks under Windows
// PowerShell 5.1, which always passes arguments the legacy way, so a double
// quote, an empty value and a final backslash after white space arrive changed
// in the form the CLI printed before. It runs on Windows only.
func TestExactValuePastePowerShell51(t *testing.T) {
	t.Parallel()
	if !testfs.OnWindows() {
		t.Skip("Windows PowerShell 5.1 runs on Windows only")
	}
	program, err := exec.LookPath("powershell.exe")
	sh := powerShell(pasteShellOrSkip(t, "powershell.exe", program, err), "")
	dir := newPasteDir(t)
	sentinel := filepath.Join(dir, "sentinel")
	assertAcceptedRows(t, sh, dir, sentinel, powerShellInjection(sentinel))
	assertRefusedRows(t)

	for _, value := range []string{`a" --yes`, "", `a b\`} {
		line := "rancher get users " + clierr.ShellQuote(value) + " 'next'"
		changed := runPaste(t, sh, dir, line)
		require.NoError(t, changed.err, "Windows PowerShell 5.1 runs the line")
		assert.NotEqual(t, []string{"get", "users", value, "next"}, changed.args, "Windows PowerShell 5.1 changes %q", value)
	}
	assert.NoFileExists(t, sentinel)
}
