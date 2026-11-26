package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// walkCommands calls fn for c and every command below it, with the words that
// name it from the root down (none for the root).
func walkCommands(c *Command, path []string, fn func(c *Command, path []string)) {
	fn(c, path)
	for _, sub := range c.Commands {
		walkCommands(sub, append(slices.Clone(path), sub.Name), fn)
	}
}

// TestTheBeforeChainRunsForTheLinesThatReachACommand asserts that the Before
// of the root runs once for a line that ends at the root or at a group, at
// a word that names no command, at a leaf, and for a completion request, and
// not for a help page, the version or a flag error. Each row captures its
// words first, as a process does, because a completion request reads the
// captured line. Not parallel: it sets the process environment and the
// captured command line.
func TestTheBeforeChainRunsForTheLinesThatReachACommand(t *testing.T) {
	exitEnv(t)
	for _, tc := range []struct {
		name  string
		words []string
		calls int
	}{
		{"a help flag", []string{"get", "pod", "--help"}, 0},
		{"a help flag at the root", []string{"--help"}, 0},
		{"a help flag before a command", []string{"--help", "get", "pod"}, 0},
		{"the version", []string{"--version"}, 0},
		{"a flag error at a leaf", []string{"get", "pod", "--bogus"}, 0},
		{"a flag error at the root", []string{"--bogus"}, 0},
		{"help after a flag error", []string{"get", "pod", "--help", "--bogus"}, 0},
		{"the root alone", nil, 1},
		{"a group alone", []string{"get"}, 1},
		{"an unknown command", []string{"nosuch"}, 1},
		{"an unknown command under a group", []string{"get", "nosuch"}, 1},
		{"an empty word", []string{""}, 1},
		{"a leaf", []string{"get", "pod"}, 1},
		{"a completion request at the root", []string{protoSentinel, curPrefix + "zzz-no-such-word", completionFlag}, 1},
		{"a completion request at a leaf", []string{protoSentinel, "get", "pod", curPrefix + "zzz-no-such-word", completionFlag}, 1},
		{"a completion request with a help flag before a command", []string{protoSentinel, "--help", "get", curPrefix, completionFlag}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := BuildRoot("v-test")
			require.NoError(t, err)
			calls := 0
			root.Before = func(context.Context, *Command, *Input) error {
				calls++
				return nil
			}
			walkCommands(root, nil, func(c *Command, _ []string) {
				if len(c.Commands) == 0 {
					c.Action = func(context.Context, *Command, *Input) error { return nil }
				}
			})
			root.Writer, root.ErrWriter = &bytes.Buffer{}, &bytes.Buffer{}
			CaptureArgv(tc.words)
			_ = root.Run(t.Context(), append([]string{"rancher"}, tc.words...))
			assert.Equal(t, tc.calls, calls)
		})
	}
}

// TestOnlyConfigReadsAnEnvironmentVariable asserts that no flag of any
// command reads an environment variable except --config, and that every flag
// has one of the three kinds (a flag literal with no kind would read as a
// string flag).
func TestOnlyConfigReadsAnEnvironmentVariable(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	walked := 0
	bound := map[string]string{}
	walkCommands(root, nil, func(c *Command, _ []string) {
		for _, f := range c.Flags {
			walked++
			assert.Contains(t, []flagKind{boolFlag, stringFlag, sliceFlag}, f.Kind, "the kind of --%s of %s", f.Name, c.Name)
			if f.EnvVar != "" {
				bound[f.Name] = f.EnvVar
			}
		}
	})
	assert.Positive(t, walked, "the walk found no flag")
	assert.Equal(t, map[string]string{"config": "RANCHER_CLI_CONFIG_DIR"}, bound)
}

// TestLeafHelpAfterAParseErrorIsItsHelpPage asserts that for every leaf,
// `<leaf> --help --bogus` writes the help page of the leaf, byte for byte as
// the golden help file records it. Not parallel: it sets the process
// environment.
func TestLeafHelpAfterAParseErrorIsItsHelpPage(t *testing.T) {
	goldenEnvironment(t)
	root, err := BuildRoot(goldenVersion)
	require.NoError(t, err)
	leaves := 0
	walkCommands(root, nil, func(c *Command, path []string) {
		if len(c.Commands) > 0 {
			return
		}
		leaves++
		var out, errOut bytes.Buffer
		root.Writer, root.ErrWriter = &out, &errOut
		require.NoError(t, root.Run(t.Context(), append(append([]string{"rancher"}, path...), "--help", "--bogus")), strings.Join(path, " "))
		want := readGolden(t, filepath.Join(goldenHelpDir, goldenName(path)+".txt"))
		assert.Equal(t, want, out.String(), "rancher %s --help --bogus", strings.Join(path, " "))
		assert.Empty(t, errOut.String(), "rancher %s --help --bogus", strings.Join(path, " "))
	})
	assert.Positive(t, leaves, "the walk found no leaf")
}

// TestCommandFlagsWinOverBuiltinAndInheritedFlags asserts, on a tree built by
// hand, that a flag a command defines wins over a flag of the same name the
// built-in flags or a command above it define: a leaf flag named version
// takes the word of its own flag and leaves the root's --version alone; a bool
// flag named like an inherited string flag takes no value; an alias of a
// command flag is dropped from the help flag, which keeps its name; and a
// command that defines --help gets no built-in help flag.
func TestCommandFlagsWinOverBuiltinAndInheritedFlags(t *testing.T) {
	t.Parallel()
	leaf := &Command{
		Name: "leaf",
		Flags: []*Flag{
			{Name: "version", Kind: stringFlag},
			{Name: "mode", Kind: boolFlag},
			{Name: "hints", Aliases: []string{"h"}, Kind: boolFlag},
		},
	}
	own := &Command{Name: "own", Flags: []*Flag{{Name: "help", Kind: boolFlag}}}
	root := link(&Command{
		Name:            "rancher",
		Version:         "v-test",
		StopAtFirstWord: true,
		Flags:           []*Flag{{Name: "mode", Kind: stringFlag}, {Name: "name", Kind: stringFlag}},
		Commands:        []*Command{leaf, own},
	})
	flagOf := func(c *Command, name string) *Flag {
		for _, f := range c.Flags {
			if slices.Contains(f.Names(), name) {
				return f
			}
		}
		return nil
	}

	p := parseLine(root, []string{"leaf", "--version", "x"})
	require.Equal(t, lineRuns, p.end.kind, "the leaf's own --version is a value flag, not the root's version flag")
	in := &Input{p: &p, level: p.end.level}
	assert.Equal(t, "x", in.String("version"))
	assert.True(t, in.IsSet("version"))

	p = parseLine(root, []string{"--version"})
	assert.Equal(t, lineVersion, p.end.kind, "the root's --version still shows the version")

	p = parseLine(root, []string{"leaf", "--mode", "y"})
	in = &Input{p: &p, level: p.end.level}
	assert.True(t, in.Bool("mode"), "the leaf's bool flag wins over the root's string flag")
	assert.Equal(t, []string{"y"}, in.Args(), "and takes no value")
	assert.Empty(t, in.String("mode"))

	p = parseLine(root, []string{"leaf", "-h"})
	in = &Input{p: &p, level: p.end.level}
	assert.Equal(t, lineRuns, p.end.kind, "an alias a command flag has is not an alias of the help flag")
	assert.True(t, in.Bool("hints"))
	p = parseLine(root, []string{"leaf", "--help"})
	assert.Equal(t, lineHelp, p.end.kind, "the help flag keeps its name")
	assert.Equal(t, []string{"help"}, flagOf(leaf, "help").Names())

	help := 0
	for _, f := range own.Flags {
		if f.Name == "help" {
			help++
		}
	}
	assert.Equal(t, 1, help, "a command that defines --help gets no built-in one")
	assert.False(t, flagOf(own, "help").Local)
	assert.Nil(t, flagOf(leaf, "v"), "the version flag is the root's alone")
}

// TestFlagNamesAreUniqueInEachCommand asserts that within one command, its own
// flags and the built-in flags have no name or alias twice.
func TestFlagNamesAreUniqueInEachCommand(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	commands := 0
	walkCommands(root, nil, func(c *Command, path []string) {
		commands++
		seen := map[string]string{}
		for _, f := range c.Flags {
			for _, name := range f.Names() {
				if other, dup := seen[name]; dup {
					assert.Failf(t, "a flag name is taken twice", "rancher %s: --%s of %s and of %s", strings.Join(path, " "), name, other, f.Name)
				}
				seen[name] = f.Name
			}
		}
	})
	assert.Positive(t, commands, "the walk found no command")
}

// TestEverySliceFlagKeepsACommaInOneValue asserts that every slice flag of
// the tree keeps a value with a comma as one entry, and one entry for each
// time it is typed.
func TestEverySliceFlagKeepsACommaInOneValue(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	found := 0
	walkCommands(root, nil, func(c *Command, path []string) {
		for _, f := range c.Flags {
			if f.Kind != sliceFlag {
				continue
			}
			found++
			long := "--" + f.Name
			p := parseLine(root, append(slices.Clone(path), long, "a,b", long+"=c,d", long, "e"))
			require.Equal(t, lineRuns, p.end.kind, "rancher %s %s", strings.Join(path, " "), long)
			in := &Input{p: &p, level: p.end.level}
			assert.Equal(t, []string{"a,b", "c,d", "e"}, sliceFlagTexts(in, f.Name), "rancher %s %s", strings.Join(path, " "), long)
		}
	})
	assert.Positive(t, found, "the walk found no slice flag")
}

// TestEveryCommandKnowsItsRoot asserts that every command of a tree has the
// root of the tree as its Root and a Lineage that ends there, and that two
// roots built from one wired registry share no command: each leaf has its
// own root as its Root.
func TestEveryCommandKnowsItsRoot(t *testing.T) {
	t.Parallel()
	check := func(root *Command) (commands []*Command) {
		walkCommands(root, nil, func(c *Command, path []string) {
			commands = append(commands, c)
			assert.Same(t, root, c.Root(), "rancher %s", strings.Join(path, " "))
			lineage := c.Lineage()
			assert.Same(t, root, lineage[len(lineage)-1], "rancher %s", strings.Join(path, " "))
			assert.Len(t, lineage, len(path)+1, "rancher %s", strings.Join(path, " "))
		})
		return commands
	}
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	assert.Greater(t, len(check(root)), 100)

	wr, err := buildWiredRegistry("v-test")
	require.NoError(t, err)
	first, second := wr.root("v-test"), wr.root("v-test")
	inFirst, inSecond := check(first), check(second)
	for _, c := range inFirst {
		assert.NotContains(t, inSecond, c, "rancher %s is in both roots", c.FullName())
	}
	assert.Len(t, inSecond, len(inFirst))
}

// TestOneTreeRunsEveryLineAsAFreshTreeDoes asserts that the tree holds no
// state of a run: every line of the parse lines of get pod and of the root
// runs on one tree, one after the other, and each gives the row the same line
// gives on a tree built for it alone. Not parallel: it sets the process
// environment and runs lines one after the other.
func TestOneTreeRunsEveryLineAsAFreshTreeDoes(t *testing.T) {
	goldenEnvironment(t)
	helps := helpsFromDisk(t, goldenCommands(t))
	checked := 0
	for _, f := range parseFiles(t) {
		if f.name != "get-pod" && f.name != "rancher" {
			continue
		}
		fresh := runGoldenLines(t, f.lines, helps)
		tree := newWrappedTree()
		require.NoError(t, tree.err)
		for i, row := range f.lines {
			tree.obs.ran, tree.obs.refused = nil, nil
			got := runGoldenLine(t, tree, row, helps)
			if !reflect.DeepEqual(fresh[i], got) {
				assert.Equal(t, fresh[i], got, "%s row %d, rancher %q", f.name, i+1, row.Line)
			}
			checked++
		}
	}
	assert.Greater(t, checked, 500, "the lines of get pod and of the root were run")
}
