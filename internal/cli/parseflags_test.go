package cli

import (
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flagTwiceWords returns the words that give flags of cmd twice: the first own
// string flag that may be given more than once, the first own bool flag and
// the first own flag with an alias. A string flag is given twice with two
// values; a bool flag bare then false and false then bare; the flag with an
// alias by its alias twice and by its alias then its long name. missing names
// each rule that found no flag.
func flagTwiceWords(cmd *Command) (lines [][]string, missing []string) {
	var text, boolean, aliased *Flag
	for _, f := range cmd.Flags {
		if f.Local {
			continue
		}
		_, once := flagShape(f)
		switch f.Kind {
		case stringFlag:
			if text == nil && !once {
				text = f
			}
			if aliased == nil && len(f.Names()) > 1 {
				aliased = f
			}
		case boolFlag:
			if boolean == nil {
				boolean = f
			}
			if aliased == nil && len(f.Names()) > 1 {
				aliased = f
			}
		}
	}
	if text == nil {
		missing = append(missing, "string flag that may be given more than once")
	} else {
		long := "--" + text.Names()[0]
		lines = append(lines, []string{long, "a", long, "b"})
	}
	if boolean == nil {
		missing = append(missing, "bool flag")
	} else {
		long := "--" + boolean.Names()[0]
		lines = append(lines, []string{long, long + "=false"}, []string{long + "=false", long})
	}
	if aliased == nil {
		missing = append(missing, "flag with an alias")
		return lines, missing
	}
	names := aliased.Names()
	long, alias := "--"+names[0], "-"+names[len(names)-1]
	if aliased.Kind == boolFlag {
		return append(lines, []string{alias, alias}, []string{alias, long + "=false"}), missing
	}
	return append(lines, []string{alias, "a", alias, "b"}, []string{alias, "a", long, "b"}), missing
}

// treeCommand is a command of the tree and the words that name it.
type treeCommand struct {
	path []string
	cmd  *Command
}

// treeCommands lists the root and every command below it.
func treeCommands(t *testing.T) (root *Command, out []treeCommand) {
	t.Helper()
	root, err := BuildRoot(goldenVersion)
	require.NoError(t, err)
	out = []treeCommand{{nil, root}}
	for _, c := range allCommands(root, nil) {
		out = append(out, treeCommand{c.path, c.cmd})
	}
	return root, out
}

// flagRef is a flag a line can name: its first name and whether a command
// above the one the line reaches defines it.
type flagRef struct {
	long      string
	flag      *Flag
	inherited bool
}

// flagsOf returns the flags the command at path accepts by every name: its
// own and those of the commands above it. A flag of the command shadows one of
// the same name above it.
func flagsOf(root *Command, path []string) map[string]flagRef {
	lineage := []*Command{root}
	for _, word := range path {
		lineage = append(lineage, lineage[len(lineage)-1].Command(word))
	}
	byName := map[string]flagRef{}
	for i, c := range slices.Backward(lineage) {
		for _, f := range c.Flags {
			for _, name := range f.Names() {
				if _, taken := byName[name]; !taken {
					byName[name] = flagRef{long: f.Names()[0], flag: f, inherited: i < len(lineage)-1}
				}
			}
		}
	}
	return byName
}

// flagUse is one occurrence of a flag on a line: its value (a string for a
// flag that takes one, a bool for a bool flag) and whether the line spelled it
// by an alias.
type flagUse struct {
	ref   flagRef
	value any
	alias bool
}

// flagUses reads the flags named by the words of line before the first "--".
// A flag that takes a value reads it from the word after it or from the text
// after "="; a bool flag is true when bare and has the parsed value after "=".
// An occurrence with no value, or a bool value that does not parse, is left
// out.
func flagUses(line []string, byName map[string]flagRef) []flagUse {
	var uses []flagUse
	for i := 0; i < len(line); i++ {
		word := line[i]
		if word == "--" {
			break
		}
		text, isFlag := strings.CutPrefix(word, "-")
		if !isFlag || text == "" {
			continue
		}
		name, value, joined := strings.Cut(strings.TrimPrefix(text, "-"), "=")
		ref, known := byName[name]
		if !known {
			continue
		}
		use := flagUse{ref: ref, alias: name != ref.long}
		takesValue, _ := flagShape(ref.flag)
		switch {
		case takesValue && joined:
			use.value = value
		case takesValue && i+1 < len(line):
			i++
			use.value = line[i]
		case takesValue:
			continue
		case !joined:
			use.value = true
		default:
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				continue
			}
			use.value = parsed
		}
		uses = append(uses, use)
	}
	return uses
}

// parseFileRows reads the rows of the parse file of the command at path.
func parseFileRows(t *testing.T, path []string) (file string, rows []parseRow) {
	t.Helper()
	file = filepath.Join(goldenParseDir, goldenName(path)+".jsonl")
	data := readGolden(t, file)
	rows, _, err := decodeParseFile([]byte(data))
	require.NoError(t, err, goldenShown(file))
	return file, rows
}

// TestFlagGivenTwiceKeepsTheLastValue asserts that for every row of kind runs
// whose line gives a string or bool flag twice with different values, the value
// the row records for the flag is the last one given. Words after the first
// "--" are not flags. The test counts the rows it checks by kind of flag and
// fails when it checks none of one kind: a change to the rows that drops a
// kind cannot make it pass.
func TestFlagGivenTwiceKeepsTheLastValue(t *testing.T) {
	t.Parallel()
	root, commands := treeCommands(t)
	checked := map[string]int{}
	for _, c := range commands {
		file, rows := parseFileRows(t, c.path)
		for i, row := range rows {
			if row.Kind != "runs" {
				continue
			}
			reached := strings.Fields(row.Command)
			byName := flagsOf(root, reached)
			byFlag := map[string][]flagUse{}
			for _, use := range flagUses(row.Line, byName) {
				if use.ref.flag.Kind != sliceFlag {
					byFlag[use.ref.long] = append(byFlag[use.ref.long], use)
				}
			}
			for _, long := range slices.Sorted(maps.Keys(byFlag)) {
				uses := byFlag[long]
				if len(uses) < 2 {
					continue
				}
				values := make([]any, len(uses))
				for j, use := range uses {
					values[j] = use.value
				}
				if !slices.ContainsFunc(values, func(v any) bool { return v != values[0] }) {
					continue
				}
				assert.Equal(t, values[len(values)-1], row.Flags[long], "%s row %d, rancher %q: --%s given %v", goldenShown(file), i+1, row.Line, long, values)
				switch uses[0].ref.flag.Kind {
				case stringFlag:
					checked["string"]++
				case boolFlag:
					checked["bool"]++
				}
				if slices.ContainsFunc(uses, func(u flagUse) bool { return u.alias }) {
					checked["alias"]++
				}
				if uses[0].ref.inherited {
					checked["inherited"]++
				}
			}
		}
	}
	for _, kind := range []string{"string", "bool", "alias", "inherited"} {
		assert.NotZero(t, checked[kind], "no row of a %s flag given twice with different values is checked", kind)
	}
	t.Logf("rows checked by kind of flag: %v", checked)
}

// TestEveryOnceFlagHasARepeatedRow asserts that every flag of every command
// that may be given once has a row of class repeated flag in the parse file of
// its command whose line gives the flag twice before any "--".
func TestEveryOnceFlagHasARepeatedRow(t *testing.T) {
	t.Parallel()
	root, commands := treeCommands(t)
	found := 0
	for _, c := range commands {
		_, rows := parseFileRows(t, c.path)
		byName := flagsOf(root, c.path)
		for _, f := range c.cmd.Flags {
			if _, once := flagShape(f); !once {
				continue
			}
			long := f.Names()[0]
			has := slices.ContainsFunc(rows, func(row parseRow) bool {
				if row.Kind != "error" || row.ErrorClass != "repeated flag" {
					return false
				}
				count := 0
				for _, use := range flagUses(row.Line, byName) {
					if use.ref.long == long {
						count++
					}
				}
				return count >= 2
			})
			assert.True(t, has, "rancher %s: --%s may be given once and has no repeated flag row that gives it twice", strings.Join(c.path, " "), long)
			if has {
				found++
			}
		}
	}
	t.Logf("%d flags that may be given once have a repeated flag row", found)
}

// TestEveryListFlagKeepsACommaValue asserts that every list flag of
// every command has a row of kind runs in the parse file of its command whose
// line gives it a value with a comma and whose flags hold that value as one
// entry.
func TestEveryListFlagKeepsACommaValue(t *testing.T) {
	t.Parallel()
	root, commands := treeCommands(t)
	found := 0
	for _, c := range commands {
		_, rows := parseFileRows(t, c.path)
		byName := flagsOf(root, c.path)
		for _, f := range c.cmd.Flags {
			if f.Kind != sliceFlag {
				continue
			}
			long := f.Names()[0]
			has := slices.ContainsFunc(rows, func(row parseRow) bool {
				if row.Kind != "runs" || row.Command != strings.Join(c.path, " ") {
					return false
				}
				entries, _ := row.Flags[long].([]any)
				for _, use := range flagUses(row.Line, byName) {
					value, isText := use.value.(string)
					if use.ref.long == long && isText && strings.Contains(value, ",") && slices.Contains(entries, any(value)) {
						return true
					}
				}
				return false
			})
			assert.True(t, has, "rancher %s: --%s is a list flag and no row of kind runs gives it a value with a comma and keeps it as one entry", strings.Join(c.path, " "), long)
			if has {
				found++
			}
		}
	}
	t.Logf("%d list flags keep a comma value as one entry", found)
}
