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
// only; a bare `-` and a number like `-1` are words, which reject the line
// at the root and groups and not at a leaf. The decision is made on the tree
// without running a completion, so every level is covered cheaply; the
// end-to-end sweep in completion_levelflags_test.go runs a few rows per
// level through urfave.
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
	}
	accepted := map[string][]string{
		"global flag with a value":         {"-o", "json"},
		"global flag with an equals value": {"--output=json"},
		"bool flag":                        {"--debug"},
		"help flag":                        {"--help"},
		"short help flag":                  {"-h"},
	}
	words := map[string][]string{
		"bare dash":            {"-"},
		"number":               {"-1"},
		"words after a number": {"-1", "--bogus"},
	}

	for path, node := range nodes {
		atRoot := path == ""
		nonLeaf := atRoot || len(node.VisibleCommands()) > 0
		for name, typed := range rejected {
			assert.True(t, lineRejected(node, typed, nonLeaf), "rancher %s %v (%s) is rejected", path, typed, name)
		}
		for name, typed := range accepted {
			assert.False(t, lineRejected(node, typed, nonLeaf), "rancher %s %v (%s) is accepted", path, typed, name)
		}
		for name, typed := range words {
			assert.Equal(t, nonLeaf, lineRejected(node, typed, nonLeaf), "rancher %s %v (%s): a word rejects the line at the root and groups only", path, typed, name)
		}
		for _, local := range [][]string{{"--version"}, {"-v"}} {
			assert.Equal(t, !atRoot, lineRejected(node, local, nonLeaf), "rancher %s %v: only the root accepts the version flag", path, local)
		}
	}
}
