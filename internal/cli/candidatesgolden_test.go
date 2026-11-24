package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var goldenCandidatesDir = filepath.Join(goldenPackageDir, "testdata", "candidates")

const (
	candidatesNext              parseSource = "next word"
	candidatesFlags             parseSource = "flag names"
	candidatesHelp              parseSource = "after --help"
	candidatesWord              parseSource = "after a word"
	candidatesUndefined         parseSource = "after an undefined flag"
	candidatesFirstFlag         parseSource = "first flag letter"
	candidatesValue             parseSource = "value position and --"
	candidatesHelpBeforeCommand parseSource = "help or version flag before a command"
	candidatesEarlierWords      parseSource = "flags and values before the word"
)

// candidateLine returns the words a shell script sends to complete the word
// cur after words: the protocol marker, the words, the current word and the
// flag that makes the binary answer with candidates.
func candidateLine(words []string, cur string) []string {
	return slices.Concat([]string{"--rancher-completion-proto=2"}, words, []string{"--rancher-completion-cur=" + cur, "--generate-shell-completion"})
}

// candidateKey names a line of a file for a set of lines.
func candidateKey(file string, line []string) string {
	return file + "\x01" + strings.Join(line, "\x00")
}

// candidateFiles lists the lines of every candidates golden file: six for each
// command, then the lines at a flag's value and after "--", the lines with a
// help or version flag before a command word, and the lines with flags, values
// and blank words before the word to complete, some with a flag name or "--" as
// the word to complete. A line that repeats another of its file is left out and
// counted in dropped.
func candidateFiles(t *testing.T) (files []goldenFile, dropped int) {
	t.Helper()
	root, err := BuildRoot(goldenVersion)
	require.NoError(t, err)
	byName := map[string]*goldenFile{}
	var ordered []*goldenFile
	seen := map[string]bool{}
	add := func(path []string, src parseSource, words []string, cur string) {
		name := goldenName(path)
		f := byName[name]
		if f == nil {
			f = &goldenFile{name: name}
			byName[name] = f
			ordered = append(ordered, f)
		}
		line := candidateLine(words, cur)
		key := candidateKey(name, line)
		if seen[key] {
			dropped++
			return
		}
		seen[key] = true
		f.lines = append(f.lines, parseRow{Line: line})
		f.sources = append(f.sources, src)
	}

	for _, path := range goldenCommands(t) {
		cmd := root
		for _, name := range path {
			cmd = cmd.Command(name)
		}
		with := func(extra ...string) []string { return append(slices.Clone(path), extra...) }
		add(path, candidatesNext, with(), "")
		add(path, candidatesFlags, with(), "--")
		add(path, candidatesHelp, with("--help"), "")
		add(path, candidatesWord, with("x"), "")
		add(path, candidatesUndefined, with("--bogus"), "")
		letter := "h"
		for _, f := range cmd.Flags {
			if f.Names()[0] == "help" {
				continue
			}
			letter = f.Names()[0][:1]
			break
		}
		add(path, candidatesFirstFlag, with(), "--"+letter)
	}

	getPod := []string{"get", "pod"}
	get := []string{"get"}
	for _, c := range []struct {
		path  []string
		src   parseSource
		words []string
		cur   string
	}{
		{nil, candidatesValue, []string{"--help"}, ""},
		{nil, candidatesValue, []string{"--version"}, ""},
		{nil, candidatesValue, []string{"-o"}, ""},
		{nil, candidatesValue, nil, "--output="},
		{getPod, candidatesValue, []string{"get", "pod", "-o"}, ""},
		{getPod, candidatesValue, getPod, "--output="},
		{getPod, candidatesValue, []string{"get", "pod", "--"}, ""},
		{getPod, candidatesValue, []string{"get", "pod", "--", "x"}, ""},
		{getPod, candidatesValue, []string{"get", "pod", "--yes="}, ""},
		{getPod, candidatesValue, []string{"get", "pod", "--wait"}, ""},

		{nil, candidatesHelpBeforeCommand, []string{"--help", "get"}, ""},
		{nil, candidatesHelpBeforeCommand, []string{"--version", "get"}, ""},
		{nil, candidatesHelpBeforeCommand, []string{"-v", "get"}, ""},
		{nil, candidatesHelpBeforeCommand, []string{"-h", "get"}, ""},
		{nil, candidatesHelpBeforeCommand, []string{"--help=true", "get"}, ""},
		{get, candidatesHelpBeforeCommand, []string{"get", "--help", "pod"}, ""},
		{get, candidatesHelpBeforeCommand, []string{"get", "-h", "pod"}, ""},

		{nil, candidatesEarlierWords, []string{"-c", "--", "completion"}, ""},
		{nil, candidatesEarlierWords, []string{"--", "x"}, ""},
		{nil, candidatesEarlierWords, []string{"-o", "json", "get"}, ""},
		{nil, candidatesEarlierWords, []string{"--yes=maybe", "get"}, ""},
		{nil, candidatesEarlierWords, []string{""}, ""},
		{nil, candidatesEarlierWords, []string{" "}, ""},
		{nil, candidatesEarlierWords, []string{"", "get"}, ""},
		{nil, candidatesEarlierWords, []string{"--help", "x"}, ""},
		{get, candidatesEarlierWords, []string{"get", "--", "x"}, ""},
		{get, candidatesEarlierWords, []string{"get", "--yes=maybe", "pod"}, ""},
		{get, candidatesEarlierWords, []string{"get", ""}, ""},
		{getPod, candidatesEarlierWords, []string{"-o", "json", "get", "pod"}, ""},
		{getPod, candidatesEarlierWords, []string{"-c", "x", "get", "pod"}, ""},
		{getPod, candidatesEarlierWords, []string{"--output=json", "get", "pod"}, ""},
		{[]string{"create", "project"}, candidatesEarlierWords, []string{"create", "project", "--quota-limit", "v", "--quota-limit", "w"}, ""},

		{[]string{"create", "project"}, candidatesEarlierWords, []string{"create", "project", "--quota-limit", "v", "--quota-limit", "w"}, "--"},
		{[]string{"create", "project"}, candidatesEarlierWords, []string{"create", "project", "--quota-limit", "v"}, "--"},
		{[]string{"create", "project"}, candidatesEarlierWords, []string{"create", "project", "--quota-limit", "v"}, "--quota"},
		{[]string{"create", "project"}, candidatesEarlierWords, []string{"create", "project", "--quota-limit=v"}, "--q"},
		{[]string{"create", "project"}, candidatesEarlierWords, []string{"create", "project", "--quota-limit", "v", "--", "x"}, "--"},
		{getPod, candidatesEarlierWords, []string{"get", "pod", "-o", "json"}, "--"},
		{getPod, candidatesEarlierWords, []string{"get", "pod", "--", "x"}, "--"},
		{getPod, candidatesEarlierWords, []string{"get", "pod", "--cluster", "x", "--", "y"}, "--c"},
		{getPod, candidatesEarlierWords, []string{"-o", "json", "get", "pod"}, "--"},
		{getPod, candidatesEarlierWords, []string{"-c", "x", "get", "pod"}, "--"},
	} {
		add(c.path, c.src, c.words, c.cur)
	}

	files = make([]goldenFile, len(ordered))
	for i, f := range ordered {
		files[i] = *f
	}
	return files, dropped
}

// assertCandidateKinds asserts that every row of a candidates file is a list
// of completion candidates: it holds only candidates, never a help page or
// the version. A candidate line is a name, or a name and its usage after a
// colon.
func assertCandidateKinds(t *testing.T, file goldenFile, rows []parseRow) {
	t.Helper()
	for i, row := range rows {
		if row.Kind != "complete" {
			assert.Fail(t, "a completion row has the wrong kind", "%s row %d, rancher %q: kind %s", file.name, i+1, row.Line, row.Kind)
		}
		for _, line := range row.Stdout {
			if strings.HasPrefix(line, "NAME:") || strings.HasPrefix(line, "rancher version ") {
				assert.Fail(t, "a completion reply holds a help page or the version", "%s row %d, rancher %q: %q", file.name, i+1, row.Line, line)
			}
		}
	}
}

// TestCandidatesGolden asserts that every completion request of the lines in
// testdata/candidates, run through Run on a tree with stand-in actions and no
// stored config (so no live completion), is answered with the candidates the
// file records: one file for each command, six lines for the command itself
// and the lines at a flag's value, after "--", with a help or version flag
// before a command word and with flags, values and blank words before the word
// to complete. Rewrite the files with -update after a change to the command
// surface or to completion and review the diff. Not parallel: it sets the
// process environment and runs lines one after the other, since they share the
// captured command line.
func TestCandidatesGolden(t *testing.T) {
	home, work := goldenEnvironment(t)
	kept := storedConfigSnapshot()
	view, _ := loadedConfigView()
	keptDir := view.ConfigDir()
	t.Cleanup(func() { setStoredConfig(kept, keptDir) })
	setStoredConfig(nil, "")

	files, dropped := candidateFiles(t)
	commands := goldenCommands(t)
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.name
	}
	require.NoError(t, checkGoldenNames(names))
	require.Len(t, files, len(commands), "every command has one candidates file")
	counts := map[string]int{}
	sources := map[parseSource]int{}
	total := 0
	for _, f := range files {
		total += len(f.lines)
		for _, s := range f.sources {
			sources[s]++
		}
	}
	t.Logf("%d lines in %d files, %d repeated lines left out; by source %v", total, len(files), dropped, sources)

	checkOutput := func(file goldenFile, rows []parseRow) {
		for i, row := range rows {
			for _, text := range row.Stdout {
				for _, dir := range []string{home, work} {
					require.NotContains(t, text, dir, "%s row %d: a candidate holds a temporary directory", file.name, i+1)
				}
			}
		}
	}

	if *update {
		helps := &goldenHelps{known: map[string]bool{}, text: recordHelp(t, commands)}
		for _, path := range commands {
			helps.known[goldenName(path)] = true
		}
		want := map[string][]byte{}
		for _, f := range files {
			first, second := recordParse(t, f, helps), recordParse(t, f, helps)
			require.NotEmpty(t, first, "no line is run for %s", f.name)
			for i := range first {
				if !reflect.DeepEqual(first[i], second[i]) {
					require.Failf(t, "two recordings differ", "file %s, rancher %q:\n%+v\n%+v", f.name, first[i].Line, first[i], second[i])
				}
				counts[first[i].Kind]++
			}
			assertCandidateKinds(t, f, first)
			checkOutput(f, first)
			require.False(t, t.Failed(), "a completion row has the wrong kind: nothing is written")
			data, err := encodeRows(first)
			require.NoError(t, err, f.name)
			want[f.name+".jsonl"] = data
		}
		writeGoldens(t, goldenCandidatesDir, want)
		t.Logf("rows by kind %v", counts)
		assertUntouched(t, home, work)
		return
	}

	helps := helpsFromDisk(t, commands)
	expected := map[string]bool{}
	for _, f := range files {
		expected[f.name+".jsonl"] = true
	}
	for _, name := range goldenDirFiles(t, goldenCandidatesDir) {
		assert.True(t, expected[name], "%s has no command: remove it or run with -update", goldenShown(filepath.Join(goldenCandidatesDir, name)))
	}
	for _, f := range files {
		t.Run(f.name, func(t *testing.T) {
			path := filepath.Join(goldenCandidatesDir, f.name+".jsonl")
			data, err := os.ReadFile(path)
			require.NoError(t, err, "no candidates file for %s: run with -update", f.name)
			for i, b := range data {
				require.Less(t, b, byte(0x80), "%s: byte %d is not ASCII", goldenShown(path), i)
			}
			want, wantFields, err := decodeParseFile(data)
			require.NoError(t, err, goldenShown(path))
			require.NotEmpty(t, want, "%s holds no row: every command has rows; run with -update", goldenShown(path))
			got := recordParse(t, f, helps)
			assertCandidateKinds(t, f, got)
			checkOutput(f, got)
			failures := 0
			for i, row := range got {
				if i >= len(want) {
					break
				}
				gotLine, err := encodeRow(row)
				require.NoError(t, err)
				gotFields, err := rowFields(gotLine)
				require.NoError(t, err)
				if reflect.DeepEqual(wantFields[i], gotFields) {
					continue
				}
				if failures++; failures <= 5 {
					assert.Equal(t, wantFields[i], gotFields, "%s row %d, rancher %q differs; run with -update if intended", goldenShown(path), i+1, row.Line)
				} else {
					t.Errorf("%s row %d, rancher %q differs; run with -update if intended", goldenShown(path), i+1, row.Line)
				}
			}
			assert.Equal(t, len(want), len(got), "%s: the number of rows differs; run with -update if intended", goldenShown(path))
		})
	}
	assertUntouched(t, home, work)
}
