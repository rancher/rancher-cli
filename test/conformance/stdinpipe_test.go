package conformance

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/testfs"
)

// stdinHelperEnv makes the copy of the test binary that stands in for rancher
// read all of stdin, print the bytes it read and its arguments, and exit. A
// command a shell pipes into rancher then shows exactly what the shell sent.
const stdinHelperEnv = "STDIN_PIPE_HELPER"

// stdinPipeTimeout ends a run whose shell hangs.
const stdinPipeTimeout = 60 * time.Second

// stdinReport is the one line of json the helper prints: its arguments and
// the bytes it read on stdin as lowercase hexadecimal. Hexadecimal keeps an
// invalid UTF-8 sequence and a byte order mark visible, which a json string
// would replace or hide.
type stdinReport struct {
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
}

// stdinHelper prints the report with every non-ASCII rune of an argument as a
// \u escape. A console code page then cannot change what the test reads back.
func stdinHelper() {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	encoded, err := json.Marshal(stdinReport{Args: os.Args[1:], Stdin: hex.EncodeToString(data)})
	if err != nil {
		os.Exit(2)
	}
	_, _ = io.WriteString(os.Stdout, escapeNonASCII(string(encoded))+"\n")
	os.Exit(0)
}

// stdinRun is one run of a script: the arguments and the stdin bytes rancher
// received, the lines the script printed itself, the stderr text of the run
// and its error (a failed shell or rancher, or output that does not have
// exactly one line of json).
type stdinRun struct {
	args   []string
	stdin  []byte
	info   []string
	stderr string
	err    error
}

// stdinLabel names the test and subtest in a log line.
func stdinLabel(t *testing.T) string {
	t.Helper()
	return strings.ReplaceAll(strings.TrimPrefix(t.Name(), "TestStdinPipe"), "/", " ")
}

// logStdinRun prints the stdin bytes of a run, for the logs of the Windows
// jobs.
func logStdinRun(t *testing.T, run stdinRun) {
	t.Helper()
	t.Logf("STDINPIPE %s: bytes=%x text=%q", stdinLabel(t), run.stdin, run.stdin)
}

// runStdinPipe writes script to a script file of the shell in dir, runs it
// with dir first on PATH, the helper mode on and env added, and returns what
// rancher received. The run ends after stdinPipeTimeout. WaitDelay closes the
// pipes of a killed shell when a child process still has them open; the copy
// of the test binary can then be removed when the test ends. The shell's stdin
// is the null device. Every line the script prints that is not the helper's
// json line goes to the log.
func runStdinPipe(t *testing.T, sh pasteShell, dir, script string, env []string) stdinRun {
	t.Helper()
	file := filepath.Join(dir, "line"+sh.suffix)
	require.NoError(t, os.WriteFile(file, []byte(sh.header+script+"\n"), 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), stdinPipeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sh.program, sh.args(file)...)
	cmd.Dir = dir
	cmd.Env = append(testfs.EnvWithoutGit(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), stdinHelperEnv+"=1")
	cmd.Env = append(cmd.Env, env...)
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	run := stdinRun{err: cmd.Run(), stderr: strings.TrimSpace(stderr.String())}

	var reports []string
	for line := range strings.SplitSeq(stdout.String(), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "{"):
			reports = append(reports, line)
		case strings.TrimSpace(line) != "":
			run.info = append(run.info, line)
			t.Logf("STDINPIPE %s info: %q", stdinLabel(t), strings.TrimPrefix(line, "info: "))
		}
	}
	if len(reports) != 1 {
		run.err = errors.Join(run.err, fmt.Errorf("want exactly one json line on stdout, got %d: %q", len(reports), stdout.String()))
		return run
	}
	var report stdinReport
	if err := json.Unmarshal([]byte(reports[0]), &report); err != nil {
		run.err = errors.Join(run.err, err)
		return run
	}
	data, err := hex.DecodeString(report.Stdin)
	if err != nil {
		run.err = errors.Join(run.err, err)
		return run
	}
	run.args, run.stdin = report.Args, data
	return run
}

// stdinPasswords are the values the POSIX examples read from the environment:
// each has a space, a dollar sign, a backslash, a percent sign, a double
// quote and a leading hyphen, and no newline.
type stdinPasswords struct {
	password, current, newPassword string
}

var stdinPipePasswords = stdinPasswords{
	password:    `-pass word$1\n%s"q`,
	current:     `-cur rent$HOME\t%d"w`,
	newPassword: `-new pa$$ss\\%x"e`,
}

func (p stdinPasswords) environment() []string {
	return []string{"PASSWORD=" + p.password, "CURRENTPASSWORD=" + p.current, "NEWPASSWORD=" + p.newPassword}
}

// stdinRow is a documented example that pipes into rancher, with the bytes
// its pipe must put on stdin. text is the example as it is in the schema
// manifest.
type stdinRow struct {
	name  string
	text  string
	stdin func(stdinPasswords) string
}

var stdinPipeRows = []stdinRow{
	{
		name:  "passwordChangeRequestNew",
		text:  `printf '%s' "$NEWPASSWORD" | rancher create password-change-request u-abc123 --password-stdin --apply --yes`,
		stdin: func(p stdinPasswords) string { return p.newPassword },
	},
	{
		name:  "passwordChangeRequestCurrentAndNew",
		text:  `printf '%s\n%s\n' "$CURRENTPASSWORD" "$NEWPASSWORD" | rancher create password-change-request u-abc123 --current-password-stdin --password-stdin --apply --yes`,
		stdin: func(p stdinPasswords) string { return p.current + "\n" + p.newPassword + "\n" },
	},
	{
		name:  "userAlice",
		text:  `printf '%s' "$PASSWORD" | rancher create user alice --password-stdin --apply --yes`,
		stdin: func(p stdinPasswords) string { return p.password },
	},
	{
		name:  "userBob",
		text:  `printf '%s' "$PASSWORD" | rancher create user bob --password-stdin --global-role user-base --apply --yes`,
		stdin: func(p stdinPasswords) string { return p.password },
	},
}

// stdinRowTexts are the example texts of the rows.
func stdinRowTexts() map[string]bool {
	texts := map[string]bool{}
	for _, row := range stdinPipeRows {
		texts[row.text] = true
	}
	return texts
}

// stdinCommandWords returns the words after "rancher" in an example.
func stdinCommandWords(text string) []string {
	_, rest, _ := strings.Cut(text, " | rancher ")
	return strings.Fields(rest)
}

// testPosixStdin runs every documented example as written under the shell and
// asserts the bytes its pipe puts on stdin and the arguments rancher gets.
// With logEcho it also logs what `echo -n` sends: a sh whose echo takes no
// options prints the option as text.
func testPosixStdin(t *testing.T, sh pasteShell, logEcho bool) {
	t.Helper()
	dir := newPasteDir(t)
	env := stdinPipePasswords.environment()
	if logEcho {
		t.Run("echoN", func(t *testing.T) {
			run := runStdinPipe(t, sh, dir, "echo -n x | rancher create user alice --password-stdin", env)
			logStdinRun(t, run)
			require.NoError(t, run.err, run.stderr)
		})
	}
	for _, row := range stdinPipeRows {
		t.Run(row.name, func(t *testing.T) {
			run := runStdinPipe(t, sh, dir, row.text, env)
			require.NoError(t, run.err, run.stderr)
			logStdinRun(t, run)
			assert.Equal(t, row.stdin(stdinPipePasswords), string(run.stdin))
			assert.Equal(t, stdinCommandWords(row.text), run.args)
		})
	}
}

// TestStdinPipeSh asserts that under POSIX sh every documented example that
// pipes a password into rancher puts exactly the password on stdin, or the
// current password and then the new password as two lines.
func TestStdinPipeSh(t *testing.T) {
	t.Parallel()
	program, err := exec.LookPath("sh")
	testPosixStdin(t, posixShell(pasteShellOrSkip(t, "sh", program, err)), true)
}

// TestStdinPipeBash asserts the same under bash.
func TestStdinPipeBash(t *testing.T) {
	t.Parallel()
	program, err := exec.LookPath("bash")
	testPosixStdin(t, posixShell(pasteShellOrSkip(t, "bash", program, err)), false)
}

// TestStdinPipeZsh asserts the same under zsh.
func TestStdinPipeZsh(t *testing.T) {
	t.Parallel()
	program, err := exec.LookPath("zsh")
	testPosixStdin(t, posixShell(pasteShellOrSkip(t, "zsh", program, err)), false)
}

// newline is what ends each item a PowerShell pipeline sends to rancher: \r\n
// on Windows, \n elsewhere.
var newline = func() string {
	if testfs.OnWindows() {
		return "\r\n"
	}
	return "\n"
}()

const (
	// powerShellPromptsHalf reads the two passwords, showing asterisks instead
	// of what is typed.
	powerShellPromptsHalf = `$current = Read-Host -MaskInput 'Current password'; $new = Read-Host -MaskInput 'New password'`

	// powerShellPipelineHalf pipes them, the current password first.
	powerShellPipelineHalf = `$current, $new | rancher create password-change-request u-abc123 --current-password-stdin --password-stdin --apply --yes`

	// powerShellExample is the two halves as one line.
	powerShellExample = powerShellPromptsHalf + "; " + powerShellPipelineHalf

	powerShellCurrent = `cur rent$HOME\x`
	powerShellNew     = `new pa$$ss\y`

	// powerShellTarget is the rancher command a logged case pipes into.
	powerShellTarget = "rancher create user alice --password-stdin --apply --yes"

	// powerShellInfo prints what the shell is and how it encodes a pipe,
	// after any setting the case made.
	powerShellInfo = `Write-Output "info: version $($PSVersionTable.PSVersion)"
Write-Output "info: encoding $($OutputEncoding.EncodingName)"
Write-Output "info: preamble $($OutputEncoding.GetPreamble().Length)"
Write-Output "info: maskinput $((Get-Command Read-Host).Parameters.ContainsKey('MaskInput'))"
`
)

// powerShellCase is one script: setup runs before the info lines, body after.
// pwsh is the check of the stdin bytes under PowerShell 7 and ps51 the check
// under Windows PowerShell 5.1; a case with no check only logs them. example
// marks the case that also asserts the arguments of the example and, under
// PowerShell 7, that Read-Host has the MaskInput parameter. reads is what the
// reader of the command the case pipes into returns for the bytes the shell
// sent, the password, or the current password and then the new password for
// the example; reads51 is the same under Windows PowerShell 5.1 and nil means
// the same as reads.
type powerShellCase struct {
	name, setup, body string
	pwsh, ps51        func(t *testing.T, stdin string)
	reads, reads51    []string
	example           bool
}

const (
	// powerShellSecret is the password the SecureString has in the SecureString
	// cases.
	powerShellSecret = "sEcr3t"

	// secureStringText is what PowerShell sends for a SecureString: the name of
	// its type.
	secureStringText = "System.Security.SecureString"

	// nonAscii is a value with one character outside ASCII, U+00E9.
	nonAscii = "b\u00e9"
)

// stdinEquals asserts the stdin bytes equal want.
func stdinEquals(want string) func(*testing.T, string) {
	return func(t *testing.T, stdin string) {
		t.Helper()
		assert.Equal(t, want, stdin)
	}
}

// stdinStartsWithByteOrderMark asserts the stdin bytes start with a UTF-8 byte
// order mark.
func stdinStartsWithByteOrderMark(t *testing.T, stdin string) {
	t.Helper()
	assert.True(t, strings.HasPrefix(stdin, bom), "stdin starts with a byte order mark: %q", stdin)
}

// powerShellCases are the cases that run before the example. Each logs what a
// pipe sends; a case with a check also asserts it.
var powerShellCases = []powerShellCase{
	{
		name: "nonAsciiDefault", setup: "$v = 'b' + [char]0xE9\n", body: "$v | " + powerShellTarget,
		// A character outside ASCII reaches rancher as UTF-8 under PowerShell 7
		// and as ? under the default $OutputEncoding of Windows PowerShell 5.1,
		// which also sends a byte order mark before the text.
		pwsh:  stdinEquals(nonAscii + newline),
		ps51:  stdinEquals(bom + "b?" + newline),
		reads: []string{nonAscii}, reads51: []string{"b?"},
	},
	{
		name: "nonAsciiUtf8NoBom", setup: "$OutputEncoding = [System.Text.UTF8Encoding]::new()\n$v = 'b' + [char]0xE9\n", body: "$v | " + powerShellTarget,
		ps51: stdinStartsWithByteOrderMark, reads: []string{nonAscii},
	},
	{
		name: "nonAsciiEncodingUtf8", setup: "$OutputEncoding = [System.Text.Encoding]::UTF8\n$v = 'b' + [char]0xE9\n", body: "$v | " + powerShellTarget,
		ps51: stdinStartsWithByteOrderMark, reads: []string{nonAscii},
	},
	{
		// A SecureString arrives as the name of its type, never as the password;
		// Windows PowerShell 5.1 sends a byte order mark before it.
		name:  "secureString",
		setup: "$s = New-Object System.Security.SecureString\nforeach ($c in '" + powerShellSecret + "'.ToCharArray()) { $s.AppendChar($c) }\n",
		body:  "$s | " + powerShellTarget,
		pwsh:  stdinEquals(secureStringText + newline),
		ps51:  stdinEquals(bom + secureStringText + newline),
		reads: []string{secureStringText},
	},
}

// powerShellExampleCase runs the pipeline half of the example with ASCII
// values that have a space, a dollar sign and a backslash.
var powerShellExampleCase = powerShellCase{
	name:    "example",
	setup:   "$current = '" + powerShellCurrent + "'\n$new = '" + powerShellNew + "'\n",
	body:    powerShellPipelineHalf,
	example: true,
	// The current password, a newline, the new password and a newline; Windows
	// PowerShell 5.1 sends a byte order mark before the current password.
	pwsh:  stdinEquals(powerShellCurrent + newline + powerShellNew + newline),
	ps51:  stdinEquals(bom + powerShellCurrent + newline + powerShellNew + newline),
	reads: []string{powerShellCurrent, powerShellNew},
}

// stdinSource is the stdin of a run as the source the password readers take.
type stdinSource []byte

func (s stdinSource) Read(string) ([]byte, error) { return s, nil }

// assertPasswordRead asserts the values the password reader of the command a
// case pipes into returns for the bytes the shell sent:
// engine.ReadCurrentAndNewPasswordStdin for the example, which has
// --current-password-stdin, and engine.ReadPasswordStdin for the other cases.
func assertPasswordRead(t *testing.T, example bool, stdin []byte, want []string) {
	t.Helper()
	if example {
		in := cmdtest.Input{Flag: map[string]bool{"password-stdin": true, "current-password-stdin": true}}
		current, newPassword, err := engine.ReadCurrentAndNewPasswordStdin(stdinSource(stdin), in)
		require.NoError(t, err)
		assert.Equal(t, want, []string{current, newPassword})
		return
	}
	in := cmdtest.Input{Flag: map[string]bool{"password-stdin": true}}
	password, err := engine.ReadPasswordStdin(stdinSource(stdin), in, "")
	require.NoError(t, err)
	assert.Equal(t, want, []string{password})
}

// testPowerShellStdin runs the cases and then the example under the shell,
// with the check of the case for that shell. A subtest that fails does not
// hide the log lines of the others.
func testPowerShellStdin(t *testing.T, sh pasteShell, version51 bool) {
	t.Helper()
	dir := newPasteDir(t)
	for _, c := range append(slices.Clone(powerShellCases), powerShellExampleCase) {
		t.Run(c.name, func(t *testing.T) {
			run := runStdinPipe(t, sh, dir, c.setup+powerShellInfo+c.body, nil)
			logStdinRun(t, run)
			require.NoError(t, run.err, run.stderr)
			check := c.pwsh
			if version51 {
				check = c.ps51
			}
			if check != nil {
				check(t, string(run.stdin))
			}
			want := c.reads
			if version51 && c.reads51 != nil {
				want = c.reads51
			}
			if want != nil {
				assertPasswordRead(t, c.example, run.stdin, want)
			}
			if c.example {
				assert.Equal(t, stdinCommandWords(powerShellPipelineHalf), run.args)
				if !version51 {
					assert.Contains(t, run.info, "info: maskinput True", "Read-Host has the MaskInput parameter")
				}
			}
		})
	}
}

// TestPowerShellExampleHasBothHalves asserts the example asks for the current
// password before the new one and pipes the current password first.
func TestPowerShellExampleHasBothHalves(t *testing.T) {
	t.Parallel()
	currentPrompt, newPrompt := strings.Index(powerShellExample, "'Current password'"), strings.Index(powerShellExample, "'New password'")
	require.GreaterOrEqual(t, currentPrompt, 0)
	assert.Less(t, currentPrompt, newPrompt)
	currentValue, newValue := strings.Index(powerShellPipelineHalf, "$current"), strings.Index(powerShellPipelineHalf, "$new")
	require.GreaterOrEqual(t, currentValue, 0)
	assert.Less(t, currentValue, newValue)
}

// TestPowerShellExampleIsInTheSkill asserts skills/rancher/SKILL.md has, in
// one code span, the whole PowerShell example the PowerShell tests run, with
// the user id placeholder the docs use.
func TestPowerShellExampleIsInTheSkill(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("skills/rancher/SKILL.md")
	require.NoError(t, err)
	want := strings.ReplaceAll(powerShellExample, "u-abc123", "<user id>")
	assert.Contains(t, markdownCodeSpans(string(raw)), want)
}

// TestStdinPipePwsh asserts four things under PowerShell 7. Piping two
// strings into rancher puts the current password, a newline, the new password
// and a newline on stdin, with the newline of the platform. A character
// outside ASCII arrives as UTF-8. A SecureString arrives as the name of its
// type. The command of the example arrives with its arguments, and Read-Host
// has the MaskInput parameter. The test also logs the other encodings. For
// each case it asserts the value the password reader of the command returns
// for the bytes the shell sent, the value the case piped, also when PowerShell 7
// sends one byte order mark because $OutputEncoding is [System.Text.Encoding]::UTF8.
func TestStdinPipePwsh(t *testing.T) {
	t.Parallel()
	program, err := exec.LookPath("pwsh")
	testPowerShellStdin(t, powerShell(pasteShellOrSkip(t, "pwsh", program, err), ""), false)
}

// TestStdinPipePowerShell51 asserts under Windows PowerShell 5.1 that it sends
// a byte order mark before piped text, that a character outside ASCII arrives
// as ? under the default $OutputEncoding, and that a SecureString arrives as
// the name of its type. For each case it asserts the value the password reader
// of the command returns for the bytes the shell sent: the one byte order mark
// it sends by default and the two it sends when $OutputEncoding is
// [System.Text.Encoding]::UTF8 are dropped; an ASCII or non-ASCII password is
// the value the case piped, and the default $OutputEncoding still gives ? for a
// non-ASCII character. It runs on Windows only.
func TestStdinPipePowerShell51(t *testing.T) {
	t.Parallel()
	if !testfs.OnWindows() {
		t.Skip("Windows PowerShell 5.1 runs on Windows only")
	}
	program, err := exec.LookPath("powershell.exe")
	testPowerShellStdin(t, powerShell(pasteShellOrSkip(t, "powershell.exe", program, err), ""), true)
}

// pipeSpan is a code span or fenced line of a Markdown file that pipes into
// rancher, with the user id placeholder replaced as in the manifest.
type pipeSpan struct {
	file, text string
}

// markdownPipeAllowed lists the code spans that pipe into rancher without a
// row of stdinPipeRows, each with the reason. An entry with an empty reason
// fails the guard. The entry texts are built from the constants the
// PowerShell tests run.
var markdownPipeAllowed = map[pipeSpan]string{
	{file: "skills/rancher/SKILL.md", text: powerShellExample}:                                                                          "PowerShell text; TestStdinPipePwsh runs its pipeline half and TestPowerShellExampleIsInTheSkill reads the whole example, both from the same constants",
	{file: "COMMANDS.md", text: strings.TrimSuffix(strings.ReplaceAll(powerShellPipelineHalf, "u-abc123", "<user>"), " --apply --yes")}: "PowerShell text, built from the pipeline TestStdinPipePwsh runs",
}

// pipeIntoRancher matches a pipe into rancher: with or without a space after
// the pipe, rancher.exe, ./rancher, .\rancher, and rancher at the end of the
// text.
var pipeIntoRancher = regexp.MustCompile(`\|\s*(\.[\\/])?rancher(\.exe)?(\s|$)`)

// inlineCodeSpans returns the code spans of one line of Markdown text: a run
// of backticks opens a span and the next run of the same length closes it.
func inlineCodeSpans(s string) []string {
	var spans []string
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		n := 1
		for i+n < len(s) && s[i+n] == '`' {
			n++
		}
		end := -1
		for j := i + n; j < len(s); {
			if s[j] != '`' {
				j++
				continue
			}
			m := 1
			for j+m < len(s) && s[j+m] == '`' {
				m++
			}
			if m == n {
				end = j
				break
			}
			j += m
		}
		if end < 0 {
			i += n
			continue
		}
		spans = append(spans, strings.TrimSpace(s[i+n:end]))
		i = end + n
	}
	return spans
}

// markdownCodeSpans returns every inline code span, wrapped lines joined, and
// every line of a fenced block, of the Markdown text. A fenced line that ends
// in a backslash or a pipe is joined with the next line.
func markdownCodeSpans(text string) []string {
	var spans, prose []string
	fenced := false
	pending := ""
	flush := func() {
		if pending != "" {
			spans = append(spans, pending)
			pending = ""
		}
	}
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~"):
			flush()
			fenced = !fenced
		case fenced:
			if trimmed == "" {
				flush()
				break
			}
			if pending != "" {
				trimmed = pending + " " + trimmed
				pending = ""
			}
			switch {
			case strings.HasSuffix(trimmed, `\`):
				pending = strings.TrimSpace(strings.TrimSuffix(trimmed, `\`))
			case strings.HasSuffix(trimmed, "|"):
				pending = trimmed
			default:
				spans = append(spans, trimmed)
			}
		default:
			prose = append(prose, line)
		}
	}
	flush()
	for _, unit := range markdownUnits(strings.Join(prose, "\n")) {
		spans = append(spans, inlineCodeSpans(unit)...)
	}
	return spans
}

// manifestForm returns an example as the schema manifest has it. A table cell
// writes the pipe as \| and the docs write the user id as the placeholder
// <user id>; the manifest has u-abc123 there.
func manifestForm(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, `\|`, "|"), "<user id>", "u-abc123")
}

// markdownPipeSpans returns the spans of text that pipe into rancher.
func markdownPipeSpans(file, text string) []pipeSpan {
	var spans []pipeSpan
	for _, span := range markdownCodeSpans(text) {
		if form := manifestForm(span); pipeIntoRancher.MatchString(form) {
			spans = append(spans, pipeSpan{file: file, text: form})
		}
	}
	return spans
}

// stdinRowProblems returns what is wrong with the rows of the examples that
// pipe into rancher. It reports a manifest example or a Markdown span that has
// no row and no entry in allowed, a row that no example uses, an entry that no
// span matches, and an entry with no reason.
func stdinRowProblems(rows map[string]bool, manifest []string, spans []pipeSpan, allowed map[pipeSpan]string) []string {
	var problems []string
	used := map[string]bool{}
	seen := map[pipeSpan]bool{}
	for _, text := range manifest {
		if !rows[text] {
			problems = append(problems, "the schema manifest has an example with no row of stdinPipeRows: "+text)
			continue
		}
		used[text] = true
	}
	for _, span := range spans {
		switch _, ok := allowed[span]; {
		case ok:
			seen[span] = true
		case rows[span.text]:
			used[span.text] = true
		default:
			problems = append(problems, span.file+" has a code span that pipes into rancher with no row of stdinPipeRows and no entry of markdownPipeAllowed: "+span.text)
		}
	}
	for text := range rows {
		if !used[text] {
			problems = append(problems, "a row of stdinPipeRows is used by no example of the manifest or the Markdown files: "+text)
		}
	}
	for span, reason := range allowed {
		if !seen[span] {
			problems = append(problems, "an entry of markdownPipeAllowed matches no code span of any file: "+span.file+": "+span.text)
		}
		if strings.TrimSpace(reason) == "" {
			problems = append(problems, "an entry of markdownPipeAllowed has no reason: "+span.file+": "+span.text)
		}
	}
	slices.Sort(problems)
	return problems
}

// manifestPipeExamples returns every example of the schema golden that pipes
// into rancher.
func manifestPipeExamples(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(schemaGolden)
	require.NoError(t, err)
	var manifest struct {
		Commands []struct {
			Examples []string `json:"examples"`
		} `json:"commands"`
	}
	require.NoError(t, json.Unmarshal(raw, &manifest))
	var out []string
	for _, c := range manifest.Commands {
		for _, example := range c.Examples {
			if pipeIntoRancher.MatchString(example) {
				out = append(out, example)
			}
		}
	}
	return out
}

// TestEveryPipedExampleHasAStdinRow asserts that every example that pipes into
// rancher, in the schema manifest or as a code span or fenced line of a
// Markdown file git lists, outside docs/ and testdata directories, has a row
// in stdinPipeRows or an entry in markdownPipeAllowed. The sh, bash and zsh
// tests run each row and assert its stdin bytes. A row or an entry that no
// example matches also fails.
func TestEveryPipedExampleHasAStdinRow(t *testing.T) {
	t.Parallel()
	manifest := manifestPipeExamples(t)
	files, err := repositoryFiles()
	require.NoError(t, err)
	var spans []pipeSpan
	for _, file := range files {
		if !strings.HasSuffix(file, ".md") || strings.HasPrefix(file, "docs/") || inTestdata(file) {
			continue
		}
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		spans = append(spans, markdownPipeSpans(file, string(raw))...)
	}
	assert.GreaterOrEqual(t, len(manifest), 4, "the manifest examples that pipe into rancher")
	assert.Contains(t, manifest, stdinPipeRows[1].text)
	assert.Contains(t, spans, pipeSpan{file: "skills/rancher/SKILL.md", text: stdinPipeRows[1].text})
	assert.Empty(t, stdinRowProblems(stdinRowTexts(), manifest, spans, markdownPipeAllowed))
}

// TestStdinRowGuardFailsOnAnUncoveredExample asserts the guard on fixed input:
// a row for every example passes, and an example, a span, a row, an entry or
// a reason without its counterpart fails.
func TestStdinRowGuardFailsOnAnUncoveredExample(t *testing.T) {
	t.Parallel()
	rows := map[string]bool{"a | rancher x": true, "b | rancher y": true}
	manifest := []string{"a | rancher x"}
	spans := []pipeSpan{{file: "f.md", text: "b | rancher y"}}
	assert.Empty(t, stdinRowProblems(rows, manifest, spans, nil))

	assert.Len(t, stdinRowProblems(rows, append(slices.Clone(manifest), "c | rancher z"), spans, nil), 1, "a manifest example with no row")
	assert.Len(t, stdinRowProblems(rows, manifest, append(slices.Clone(spans), pipeSpan{file: "g.md", text: "c | rancher z"}), nil), 1, "a span with no row")
	assert.Len(t, stdinRowProblems(rows, nil, spans, nil), 1, "a row that no example uses")
	allowed := map[pipeSpan]string{{file: "g.md", text: "c | rancher z"}: "reason"}
	assert.Len(t, stdinRowProblems(rows, manifest, spans, allowed), 1, "an entry that matches no span")
	withSpan := append(slices.Clone(spans), pipeSpan{file: "g.md", text: "c | rancher z"})
	assert.Empty(t, stdinRowProblems(rows, manifest, withSpan, allowed))
	noReason := map[pipeSpan]string{{file: "g.md", text: "c | rancher z"}: " "}
	assert.Len(t, stdinRowProblems(rows, manifest, withSpan, noReason), 1, "an entry with no reason")
}

// TestMarkdownPipeSpans asserts the spans the guard reads: an inline span, a
// span wrapped over two lines, a table cell with an escaped pipe, a fenced
// line, and the user id placeholder; prose without a code span is left out.
func TestMarkdownPipeSpans(t *testing.T) {
	t.Parallel()
	text := "Use `a | rancher get <user id>` and `b |\n  rancher get x`.\n\n| cmd | text |\n| --- | --- |\n| c | `d \\| rancher get y` |\n\n```\ne | rancher get z\n```\n\nPlain a | rancher text and `no pipe here`.\n"
	assert.Equal(t, []pipeSpan{
		{"f.md", "e | rancher get z"},
		{"f.md", "a | rancher get u-abc123"},
		{"f.md", "b | rancher get x"},
		{"f.md", "d | rancher get y"},
	}, markdownPipeSpans("f.md", text))
}

// TestMarkdownPipeSpansReadTheSpellings asserts the guard reads a pipe into
// rancher in these forms: no space after the pipe, rancher at the end of the
// span, rancher.exe, ./rancher, .\rancher, and a fenced command that
// continues on the next line after a backslash or a pipe. A pipe into another
// command whose name starts with rancher, such as rancher-cli, is left out.
func TestMarkdownPipeSpansReadTheSpellings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, text, want string
	}{
		{"no space", "`a |rancher get x`", "a |rancher get x"},
		{"end of span", "`a | rancher`", "a | rancher"},
		{"exe", "`a | rancher.exe get x`", "a | rancher.exe get x"},
		{"dot slash", "`a | ./rancher get x`", "a | ./rancher get x"},
		{"dot backslash", "`a | .\\rancher get x`", `a | .\rancher get x`},
		{"backslash continuation", "```\na | \\\n  rancher get x\n```\n", "a | rancher get x"},
		{"pipe continuation", "```\na |\n  rancher get x\n```\n", "a | rancher get x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, []pipeSpan{{"f.md", tc.want}}, markdownPipeSpans("f.md", tc.text))
		})
	}
	for _, text := range []string{"`a | rancher-cli get x`", "`a | ranchers get x`", "`a | cat`", "`rancher get x`"} {
		assert.Empty(t, markdownPipeSpans("f.md", text), text)
	}
}
