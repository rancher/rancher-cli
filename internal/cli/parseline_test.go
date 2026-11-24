package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// lineShape is a few words typed on a command line and the rule of reading
// them that they exercise.
type lineShape struct {
	rule  string
	words []string
}

// shapes are the words every command is run with, after its name and, for
// the commands that name others, ahead of it and between its names.
var shapes = []lineShape{
	{"a blank word is an argument", []string{""}},
	{"a space is an argument", []string{" "}},
	{"a tab is an argument", []string{"\t"}},
	{"a word is an argument", []string{"x"}},
	{"a padded word is an argument as typed", []string{" x "}},
	{"two blank words then a lone -", []string{"", "", "-"}},
	{"two blank words, a lone - and a word", []string{"", "", "-", "x"}},
	{"a blank word then a lone -", []string{"", "-"}},
	{"a blank word then an undefined flag", []string{"", "--bogus"}},
	{"a blank word then a command name", []string{"", "get"}},
	{"a blank word then the help flag", []string{"", "--help"}},
	{"a space, a lone - and a word", []string{" ", "-", "x"}},
	{"a command name", []string{"get"}},
	{"a padded command name is an unknown command", []string{" get"}},
	{"an unknown command", []string{"nosuch"}},
	{"a lone - keeps parsing at a leaf", []string{"-"}},
	{"a padded lone - is read as one", []string{" - "}},
	{"a lone - with a tab", []string{"\t-"}},
	{"a lone - and a word", []string{"-", "x"}},
	{"a padded lone - and a word", []string{" - ", "x"}},
	{"a lone - and the help flag", []string{"-", "--help"}},
	{"a lone - and an undefined flag", []string{"-", "--bogus"}},
	{"a - and a space and a word", []string{"- x"}},
	{"-- ends the flags", []string{"--"}},
	{"a padded -- ends the flags", []string{" -- "}},
	{"words after -- are arguments", []string{"--", "--help"}},
	{"a lone - after --", []string{"--", "-", "x"}},
	{"a flag after --", []string{"--", "--yes="}},
	{"a number is not a flag", []string{"-1"}},
	{"words after a number are arguments", []string{"-1", "--help"}},
	{"a dash and a symbol is not a flag", []string{"-€"}},
	{"a dash and an accented letter is a flag", []string{"-\u00e9"}},
	{"three dashes cut two", []string{"---x"}},
	{"three dashes before a defined flag", []string{"---yes"}},
	{"three dashes before the help flag", []string{"---help"}},
	{"an empty flag name", []string{"--=x"}},
	{"a dash and = is not a flag", []string{"-="}},
	{"the help flag", []string{"--help"}},
	{"the short help flag", []string{"-h"}},
	{"the help flag with one dash", []string{"-help"}},
	{"the help flag off", []string{"--help=false"}},
	{"the help flag with an empty value", []string{"--help="}},
	{"the help flag with a blank value", []string{"--help= "}},
	{"the short help flag with an empty value", []string{"-h="}},
	{"the help flag on", []string{"--help=true"}},
	{"the version flag", []string{"--version"}},
	{"the short version flag", []string{"-v"}},
	{"the version flag off", []string{"--version=false"}},
	{"the version flag with a blank value", []string{"--version= "}},
	{"an undefined flag", []string{"--bogus"}},
	{"a bool flag", []string{"--yes"}},
	{"a bool flag with an empty value", []string{"--yes="}},
	{"a bool flag with a blank value", []string{"--yes= "}},
	{"a bool flag with a tab value", []string{"--yes=\t"}},
	{"a bool flag off", []string{"--yes=false"}},
	{"a bool flag with a value that is not a bool", []string{"--yes=maybe"}},
	{"a bool value as its own word is an argument", []string{"--yes", "false"}},
	{"a value flag with no value", []string{"--output"}},
	{"a value flag and its value", []string{"-o", "json"}},
	{"a value flag joined to its value", []string{"-o=json"}},
	{"a value flag with a lone - as its value", []string{"-o", "-"}},
	{"a value flag with the help flag as its value", []string{"-o", "--help"}},
	{"a padded flag", []string{" --yes "}},
}

// coreShapes are the words paired with each other on the commands that get
// pairs.
var coreShapes = []lineShape{
	{"a word", []string{"x"}},
	{"a lone -", []string{"-"}},
	{"a padded lone -", []string{" - "}},
	{"--", []string{"--"}},
	{"a number", []string{"-1"}},
	{"the help flag", []string{"--help"}},
	{"the help flag off", []string{"--help=false"}},
	{"the version flag", []string{"--version"}},
	{"an undefined flag", []string{"--bogus"}},
	{"a bool flag with a blank value", []string{"--yes= "}},
	{"a bool flag with an empty value", []string{"--yes="}},
	{"a bool flag", []string{"--yes"}},
	{"a value flag and its value", []string{"-o", "json"}},
	{"a command name", []string{"get"}},
	{"an unknown command", []string{"nosuch"}},
}

// flagShape returns the shape of f: whether it takes a value and whether it
// may be given once.
func flagShape(f *Flag) (takesValue, once bool) {
	return f.takesValue(), f.Once
}

// flagShapes returns the lines that spell each of flags every way it
// reads: a value flag with its value after it, joined to it and by its
// other name, with no value at the end of the line; a bool flag bare, with
// an empty value, with false, with a blank value and by its other name; a
// flag that may be given once given twice.
func flagShapes(flags []*Flag) []lineShape {
	var out []lineShape
	missing := false
	for _, f := range flags {
		if f.Local {
			continue
		}
		names := f.Names()
		long := "--" + names[0]
		other := "-" + names[len(names)-1]
		takesValue, once := flagShape(f)
		if takesValue {
			out = append(out,
				lineShape{"a value flag and its value", []string{long, "v"}},
				lineShape{"a value flag joined to its value", []string{long + "=v"}},
				lineShape{"a value flag by its other name", []string{other, "v"}},
			)
			if !missing {
				missing = true
				out = append(out, lineShape{"a value flag with no value at the end", []string{long}})
			}
			if once {
				out = append(out, lineShape{"a flag given once, given twice", []string{long, "v", long, "w"}})
			}
			continue
		}
		out = append(out,
			lineShape{"a bool flag", []string{long}},
			lineShape{"a bool flag with an empty value", []string{long + "="}},
			lineShape{"a bool flag off", []string{long + "=false"}},
			lineShape{"a bool flag with a blank value", []string{long + "= "}},
			lineShape{"a bool flag by its other name", []string{other}},
		)
		if once {
			out = append(out, lineShape{"a flag given once, given twice", []string{long, long}})
		}
	}
	return out
}

// lineCase is one command line, with the rule it exercises.
type lineCase struct {
	rule string
	line []string
}

// blankShapes are a blank word and a padded blank word, which count toward
// the number of arguments a root or a group takes before it stops reading
// flags.
var blankShapes = []lineShape{
	{"a blank word", []string{""}},
	{"a space", []string{" "}},
}

// lineTier says which lines a command is run with, besides the spellings of
// its own flags: the shapes after its name and ahead of its names, the
// spellings of the global flags, pairs of shapes, and pairs of a blank word
// and another shape.
type lineTier struct {
	shapes, globals, pairs, blankPairs bool
}

// casesFor returns the lines the test runs on the command at path.
func casesFor(path []string, cmd *Command, rootFlags []*Flag, tier lineTier) []lineCase {
	seen := map[string]bool{}
	var out []lineCase
	add := func(rule string, words ...[]string) {
		line := slices.Concat(words...)
		key := strings.Join(line, "\x00")
		if !seen[key] {
			seen[key] = true
			out = append(out, lineCase{rule, line})
		}
	}
	if tier.shapes {
		for _, s := range shapes {
			add(s.rule, path, s.words)
		}
		for _, s := range slices.Concat(coreShapes, blankShapes) {
			for k := range len(path) {
				add(s.rule+" ahead of a name", path[:k], s.words, path[k:])
			}
		}
	}
	pairShapes := append(slices.Clone(coreShapes[:9]), blankShapes...)
	if tier.pairs {
		for _, a := range pairShapes {
			for _, b := range pairShapes {
				add(a.rule+", then "+b.rule, path, a.words, b.words)
			}
		}
	}
	if tier.blankPairs {
		for _, a := range blankShapes {
			for _, b := range pairShapes {
				add(a.rule+", then "+b.rule, path, a.words, b.words)
				add(b.rule+", then "+a.rule, path, b.words, a.words)
			}
		}
	}
	if tier.globals {
		for _, s := range flagShapes(rootFlags) {
			add(s.rule, path, s.words)
			add(s.rule+" ahead of the names", s.words, path)
		}
	}
	for _, s := range flagShapes(cmd.Flags) {
		add(s.rule, path, s.words)
	}
	return out
}

// allCommands lists every command of the tree under root with the words that
// name it.
func allCommands(root *Command, path []string) (out []struct {
	path []string
	cmd  *Command
}) {
	for _, sub := range root.Commands {
		p := append(slices.Clone(path), sub.Name)
		out = append(out, struct {
			path []string
			cmd  *Command
		}{p, sub})
		out = append(out, allCommands(sub, p)...)
	}
	return out
}

// parseTarget is a command, given by the words that name it (none for the
// root), and the tier of command lines it is run with.
type parseTarget struct {
	path []string
	tier lineTier
}

// parseTargets lists the root and every command below it, with the lines
// each is run with: the shapes, spellings of the global flags and pairs for
// the root, every group, "get pod", "create project", the first leaf with a
// string slice flag and the first leaf with a flag that may be given once;
// pairs of a blank word and another shape for the other groups; the spellings
// of its own flags only for every other leaf. It also returns the names of
// the leaves with a string slice flag and with a flag that may be given once.
func parseTargets(t *testing.T, root *Command) (targets []parseTarget, withSlice, withOnce string) {
	t.Helper()
	commands := allCommands(root, nil)
	keys := map[string]bool{}
	for _, name := range []string{"get pod", "create project"} {
		keys[name] = true
	}
	for _, c := range commands {
		if len(c.cmd.Commands) > 0 {
			continue
		}
		for _, f := range c.cmd.Flags {
			_, once := flagShape(f)
			if f.Kind == sliceFlag && withSlice == "" {
				withSlice = strings.Join(c.path, " ")
			}
			if once && withOnce == "" {
				withOnce = strings.Join(c.path, " ")
			}
		}
	}
	keys[withSlice], keys[withOnce] = true, true
	require.NotEmpty(t, withSlice, "a leaf with a string slice flag")
	require.NotEmpty(t, withOnce, "a leaf with a flag that may be given once")

	targets = []parseTarget{{nil, lineTier{shapes: true, globals: true, pairs: true}}}
	for _, c := range commands {
		name := strings.Join(c.path, " ")
		tier := lineTier{}
		if len(c.cmd.Commands) > 0 || keys[name] {
			tier.shapes = true
			tier.globals = name == "get" || keys[name]
			tier.pairs = tier.globals
			tier.blankPairs = !tier.pairs && len(c.cmd.Commands) > 0
		}
		targets = append(targets, parseTarget{c.path, tier})
	}
	return targets, withSlice, withOnce
}
