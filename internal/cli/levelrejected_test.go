package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// levelCommands returns every command in the built tree with its argv path,
// after one run of the root has set the tree up: urfave appends the help
// flag (and, at the root, the version flag) to every command during that
// setup, so the flags of each node are what completion sees. The root has an
// empty path.
func levelCommands(t *testing.T) map[string]*ucli.Command {
	t.Helper()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &out
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}
	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--help"}))

	nodes := map[string]*ucli.Command{"": root}
	var walk func(c *ucli.Command, path []string)
	walk = func(c *ucli.Command, path []string) {
		for _, sub := range c.Commands {
			if sub.Hidden {
				continue
			}
			p := append(slices.Clone(path), sub.Name)
			nodes[strings.Join(p, " ")] = sub
			walk(sub, p)
		}
	}
	walk(root, nil)
	return nodes
}

// TestLineRejectedAtEveryLevel asserts the decision completion makes, for
// the root, every group and every leaf of the built tree, on the words typed
// after the command path: a flag the level does not accept rejects the line
// wherever it sits (last word, `=`-valued, with its value attached
// (`-ojson`), followed by a global flag, or followed by a flag that waits for
// its value); flags the level accepts do not (a global flag with a value in
// either spelling, --help and -h); --version and -v are accepted at the root
// only; a bare `-` rejects the line at every level, alone or after other
// flags, since no word after it is read; a number like `-1` is a word,
// which rejects the line once there are more words than the level takes
// positional arguments:
// none at the root and groups, the declared Args at a leaf (as many plain
// words as a leaf declares are accepted, one more is rejected). A word
// typed after a bool flag that accepts it as its value rejects the line at
// every level, also when the level takes a positional argument; the same
// word joined (`--debug=true`), a word the flag does not accept, and a
// word after `--` (a `-` there too) are counted as words; a `-` as a
// flag's value is a value. A bool flag typed with "=" and an empty value
// (`--debug=`) and a bare `-` reject the line at every level, after the
// command path or ahead of it; after `--` each is a word. The decision is
// made on the tree without running a completion, so every level is covered
// cheaply; the end-to-end sweep in completion_levelflags_test.go runs a few
// rows per level through urfave.
func TestLineRejectedAtEveryLevel(t *testing.T) {
	t.Parallel()
	nodes := levelCommands(t)
	require.Greater(t, len(nodes), 100)

	rejected := map[string][]string{
		"undefined flag last":                 {"--bogus"},
		"undefined flag with a value":         {"--bogus=1"},
		"short flag with an attached value":   {"-ojson"},
		"undefined flag then a global flag":   {"--bogus", "--debug"},
		"undefined flag then a pending value": {"--bogus", "-o"},
		"global flag then an undefined flag":  {"-o", "json", "--bogus"},
		"bare dash then an undefined flag":    {"-", "--bogus"},
		"bare dash":                           {"-"},
		"a global flag then a bare dash":      {"-o", "json", "-"},
		"bool flag then a value it accepts":   {"--debug", "true"},
		"bool flag then a numeric bool value": {"--debug", "0"},
		"bool flag with an empty value":       {"--debug="},
		"empty bool value after other flags":  {"-o", "json", "--debug="},
	}
	accepted := map[string][]string{
		"global flag with a value":         {"-o", "json"},
		"global flag with an equals value": {"--output=json"},
		"bool flag":                        {"--debug"},
		"bool flag with a joined value":    {"--debug=true"},
		"bool flag joined with false":      {"--debug=false"},
		"help flag":                        {"--help"},
		"short help flag":                  {"-h"},
		"a dash as a flag's value":         {"--project", "-"},
	}
	words := map[string][]string{
		"number":               {"-1"},
		"words after a number": {"-1", "--bogus"},
	}

	m, err := buildManifest("v-test")
	require.NoError(t, err)
	declared := map[string]int{}
	for _, c := range m.Commands {
		declared[c.Name] = len(c.Args)
	}

	for path, node := range nodes {
		// The command line lineRejected reads: the command path, then the
		// words typed after it.
		line := func(typed []string) []string { return append(strings.Fields(path), typed...) }
		atRoot := path == ""
		nonLeaf := atRoot || len(node.VisibleCommands()) > 0
		maxWords := 0
		if !nonLeaf {
			n, ok := declared[path]
			require.True(t, ok, "leaf %q is in the manifest", path)
			maxWords = n
		}
		for name, typed := range rejected {
			assert.True(t, lineRejected(node, line(typed), maxWords), "rancher %s %v (%s) is rejected", path, typed, name)
		}
		for name, typed := range accepted {
			assert.False(t, lineRejected(node, line(typed), maxWords), "rancher %s %v (%s) is accepted", path, typed, name)
		}
		for name, typed := range words {
			assert.Equal(t, len(typed) > maxWords, lineRejected(node, line(typed), maxWords),
				"rancher %s %v (%s): words reject the line past the %d positional arguments the level takes", path, typed, name, maxWords)
		}
		for _, local := range [][]string{{"--version"}, {"-v"}} {
			assert.Equal(t, !atRoot, lineRejected(node, line(local), maxWords), "rancher %s %v: only the root accepts the version flag", path, local)
		}
		// An empty bool value typed ahead of the command path rejects the
		// line too.
		assert.True(t, lineRejected(node, append([]string{"--debug="}, line(nil)...), maxWords), "rancher --debug= %s is rejected", path)
		// So does a bare dash, after which the command path is not read.
		assert.True(t, lineRejected(node, append([]string{"-"}, line(nil)...), maxWords), "rancher - %s is rejected", path)
		// One word each: a bool value joined to its flag, a word the bool
		// flag does not accept, and a bool value or an empty bool value
		// after "--".
		for _, typed := range [][]string{{"--debug=true", "x"}, {"--debug", "maybe"}, {"--debug", "--", "true"}, {"--", "--debug="}, {"--", "-"}} {
			assert.Equal(t, maxWords < 1, lineRejected(node, line(typed), maxWords),
				"rancher %s %v: one word, rejected only past the %d positional arguments the level takes", path, typed, maxWords)
		}
		plain := slices.Repeat([]string{"x"}, maxWords)
		assert.False(t, lineRejected(node, line(plain), maxWords), "rancher %s %v: the declared positional arguments are accepted", path, plain)
		assert.True(t, lineRejected(node, line(append(plain, "x")), maxWords), "rancher %s %v: one more positional argument is rejected", path, append(plain, "x"))
	}
}
