package conformance

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/testfs"
)

// printedWarnings returns the lines a result with the given warnings writes to
// standard error, each without its "# warning: " prefix and line end.
func (e terminalEnv) printedWarnings(warnings ...string) []string {
	e.t.Helper()
	res := runtime.Result{Text: "ok\n", Warnings: warnings}
	_, errOut := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeText), res: res}, nil)
	var lines []string
	for line := range strings.SplitSeq(strings.TrimSuffix(errOut, "\n"), "\n") {
		lines = append(lines, strings.TrimPrefix(line, "# warning: "))
	}
	require.Len(e.t, lines, len(warnings))
	return lines
}

// singleQuoteIsInADoubleQuotedPiece reports whether a single quote of line is
// inside a double-quoted piece, the way a POSIX shell reads the line outside
// single quotes.
func singleQuoteIsInADoubleQuotedPiece(line string) bool {
	inSingle, inDouble := false, false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case inSingle:
			inSingle = c != '\''
		case inDouble:
			switch c {
			case '\\':
				i++
			case '"':
				inDouble = false
			case '\'':
				return true
			}
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		}
	}
	return false
}

// TestAWarningNeverPutsACommandArgumentInDoubleQuotes asserts a warning whose
// command argument holds a shell substitution, after a clause that holds a
// hidden character, prints the argument inside single quotes and never inside
// double quotes, and that a shell that runs the printed line creates no file.
func TestAWarningNeverPutsACommandArgumentInDoubleQuotes(t *testing.T) {
	t.Parallel()
	testfs.SkipOnWindows(t, "the printed line is run by a POSIX shell")
	e := newTerminalEnv(t)
	pwned := filepath.Join(e.dir, "pwned")
	warning := "a\u200bb; run " + clierr.Rancher + " get z\u200bz 'x$(: > " + pwned + ")'"
	line := e.printedWarnings(warning)[0]

	assert.False(t, singleQuoteIsInADoubleQuotedPiece(line), "no double-quoted piece holds a single quote: %q", line)
	found := false
	for i := range len(line) {
		if !strings.HasPrefix(line[i:], "$(") {
			continue
		}
		found = true
		assert.Equal(t, 1, strings.Count(line[:i], "'")%2, "the substitution at %d is between single quotes: %q", i, line)
	}
	assert.True(t, found, "the argument is in the line: %q", line)

	look, lookErr := exec.LookPath("sh")
	program := pasteShellOrSkip(t, "sh", look, lookErr)
	cmd := exec.CommandContext(t.Context(), program, "-c", line)
	cmd.Dir = t.TempDir()
	cmd.Env = []string{"PATH=" + t.TempDir()}
	_ = cmd.Run()
	assert.NoFileExists(t, pwned, "the shell ran the substitution")
}

// TestAProseValueThatReadsAsASubstitutionIsPrintedInDoubleQuotes asserts the
// printed form of a warning whose value holds a substitution outside single
// quotes, followed by a hidden character: the whole clause is one
// double-quoted string with escapes. Pasted into a shell, that line would run
// the substitution. This test does not run it; it records the printed form.
func TestAProseValueThatReadsAsASubstitutionIsPrintedInDoubleQuotes(t *testing.T) {
	t.Parallel()
	e := newTerminalEnv(t)
	warning := "\\$(: > " + e.dir + "/pwned)\u200b"
	assert.Equal(t, []string{strconv.Quote(warning)}, e.printedWarnings(warning))
}

// TestWarningsInJSONAndYAMLCarryTheValueAsItIs asserts json and yaml carry a
// warning that holds hidden characters exactly as the command wrote it, for a
// result and for a preview.
func TestWarningsInJSONAndYAMLCarryTheValueAsItIs(t *testing.T) {
	t.Parallel()
	want := terminalHostile("warnings json")
	decode := func(t *testing.T, format, out string) []string {
		t.Helper()
		var doc struct {
			Warnings []string `json:"warnings"`
		}
		switch format {
		case "json":
			require.NoError(t, json.Unmarshal([]byte(out), &doc))
		default:
			require.NoError(t, yaml.Unmarshal([]byte(out), &doc))
		}
		return doc.Warnings
	}
	for _, format := range []string{"json", "yaml"} {
		t.Run("result "+format, func(t *testing.T) {
			t.Parallel()
			e := newTerminalEnv(t)
			res := runtime.Result{Kind: "Report", Data: terminalTable{cols: []string{"NAME"}, rows: [][]string{{"a"}}}, Warnings: []string{want}}
			out, _ := e.run(terminalRead{d: terminalDescriptor(runtime.ShapeTabular), res: res}, func(io *runtime.IO) { io.Format = format })
			assert.Equal(t, []string{want}, decode(t, format, out))
		})
		t.Run("preview "+format, func(t *testing.T) {
			t.Parallel()
			e := newTerminalEnv(t)
			eff := safety.Effect{PreviewSource: safety.PreviewServerDryRun, Action: safety.ActionUpdate, DisplayKind: "Cluster", DisplayName: "c", Warnings: []string{want}}
			out, _ := e.run(terminalMutating{d: previewDescriptor(), eff: eff}, func(io *runtime.IO) { io.DryRun = true; io.Format = format })
			assert.Equal(t, []string{want}, decode(t, format, out))
		})
	}
}

// TestACommandPrintsTheSameBesideAHiddenCharacter asserts two things. A remedy
// command prints as it does in a warning with nothing to quote when a clause
// with a hidden character is next to it, for a config directory with a
// Windows-style path. A directory whose path cannot be printed leaves the
// command out beside a hidden character, as it does without one.
func TestACommandPrintsTheSameBesideAHiddenCharacter(t *testing.T) {
	t.Parallel()
	after := "value a\u200bb; run " + clierr.Rancher + " auth status"
	before := "x\u200b run " + clierr.Rancher + " auth status"
	clean := "run " + clierr.Rancher + " auth status"

	t.Run("a path that prints", func(t *testing.T) {
		t.Parallel()
		dir := `C:\Users\obrien\rancher`
		prefix, ok := runtime.RemedyPrefix(dir)
		require.True(t, ok)
		e := terminalEnv{t: t, dir: dir, expanded: "rancher" + prefix + " auth status"}
		lines := e.printedWarnings(after, before, clean)
		cleanLine := lines[2]
		assert.Equal(t, "run "+e.expanded, cleanLine)
		assert.Equal(t, `"value a\u200bb"; `+cleanLine, lines[0])
		assert.Equal(t, `"x\u200b run "`+strings.TrimPrefix(cleanLine, "run "), lines[1])
	})

	t.Run("a path that cannot be printed", func(t *testing.T) {
		t.Parallel()
		dir := `C:\Users\o'brien\rancher`
		_, ok := runtime.RemedyPrefix(dir)
		require.False(t, ok)
		e := terminalEnv{t: t, dir: dir}
		lines := e.printedWarnings(after, before)
		assert.Equal(t, `"value a\u200bb"; `+runtime.UnprintableConfigDirNote, lines[0])
		assert.Equal(t, runtime.UnprintableConfigDirNote, lines[1])
	})
}
