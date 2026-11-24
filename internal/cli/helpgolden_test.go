package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordHelp returns the help of each command in commands by file name: what
// `<command> --help` writes to standard output through Run, with no color. A
// command that shows anything else, or writes to standard error, fails the
// test.
func recordHelp(t *testing.T, commands [][]string) map[string]string {
	t.Helper()
	rows := make([]parseRow, len(commands))
	for i, path := range commands {
		rows[i] = parseRow{Line: append(slices.Clone(path), "--help")}
	}
	text := map[string]string{}
	for i, o := range runGoldenLines(t, rows, nil) {
		label := "rancher " + strings.Join(rows[i].Line, " ")
		require.Equal(t, "help", o.row.Kind, label)
		require.Equal(t, strings.Join(commands[i], " "), o.row.Command, label)
		require.Zero(t, o.row.Exit, label)
		require.Empty(t, o.row.Stderr, label)
		require.NotContains(t, o.stdout, "\r", label)
		text[goldenName(commands[i])] = o.stdout
	}
	return text
}

// TestHelpGolden asserts that every command, the root included, shows the
// help in its file under testdata/help when it is run with --help, and that
// every file belongs to a command. Rewrite the files with
// -update after a change to the command surface and review the diff. Not
// parallel: it sets the process environment and the captured command line.
func TestHelpGolden(t *testing.T) {
	home, work := goldenEnvironment(t)
	commands := goldenCommands(t)
	names := make([]string, len(commands))
	for i, path := range commands {
		names[i] = goldenName(path)
	}
	require.NoError(t, checkGoldenNames(names))
	got := recordHelp(t, commands)

	if *update {
		again := recordHelp(t, commands)
		for _, name := range names {
			require.Equal(t, got[name], again[name], "two recordings of the help of %s differ", name)
		}
		want := map[string][]byte{}
		for name, text := range got {
			want[name+".txt"] = []byte(text)
		}
		writeGoldens(t, goldenHelpDir, want)
		assertUntouched(t, home, work)
		return
	}

	for _, file := range goldenDirFiles(t, goldenHelpDir) {
		name, isHelp := strings.CutSuffix(file, ".txt")
		assert.True(t, isHelp && slices.Contains(names, name), "%s has no command: remove it or run with -update", goldenShown(filepath.Join(goldenHelpDir, file)))
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(goldenHelpDir, name+".txt")
			want := readGolden(t, path)
			assert.Equal(t, want, got[name], "the help of %s differs from %s; run with -update if intended", name, goldenShown(path))
		})
	}
	assertUntouched(t, home, work)
}

// readGolden returns the text of a golden file with its line ends as \n, for a
// checkout that made them \r\n.
func readGolden(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "no file %s: run with -update", goldenShown(path))
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}
