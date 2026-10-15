package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

// TestEveryLeafCarriesItsDescriptor asserts that every command of the built
// tree that has no subcommands holds the runtime.Descriptor lineRefusal
// reads, hidden commands included, so a line that reaches a leaf is always
// checked for a bool value typed as its own word.
func TestEveryLeafCarriesItsDescriptor(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	wired, err := buildWiredRegistry("v-test", config.ReadOnlyOverride)
	require.NoError(t, err)
	registered := len(wired.fixed)
	for _, leaves := range wired.groups {
		registered += len(leaves)
	}
	leaves := 0
	for _, c := range allCommands(root, nil) {
		if len(c.cmd.Commands) > 0 {
			continue
		}
		leaves++
		d, ok := commandDescriptor(c.cmd)
		if assert.True(t, ok, "%v has a Descriptor", c.path) {
			assert.Equal(t, c.cmd.Name, leafName(d.Name), "%v", c.path)
		}
	}
	assert.Equal(t, registered, leaves, "every leaf the registry wires is one the tree holds")
	assert.NotZero(t, leaves)
}

// TestALeafWithoutADescriptorIsAnInternalError asserts that a line that
// reaches a leaf built without a Descriptor and runs it is not let through
// unchecked: it fails with a GENERIC error naming the command, exit 1,
// written to the error writer. A parse error, help or version urfave
// reaches first wins, as it does over every refusal.
func TestALeafWithoutADescriptorIsAnInternalError(t *testing.T) {
	restoreRawArgv(t)
	newRoot := func() *ucli.Command {
		return &ucli.Command{
			Name:    "rancher",
			Version: "v-test",
			Commands: []*ucli.Command{{
				Name:   "bare",
				Flags:  []ucli.Flag{&ucli.BoolFlag{Name: "apply"}},
				Action: func(context.Context, *ucli.Command) error { return nil },
			}},
		}
	}

	ce, wholeLine := lineRefusal(parseLine(newRoot(), []string{"bare", "--apply", "false"}))
	require.NotNil(t, ce)
	assert.False(t, wholeLine)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Equal(t, "internal error: rancher bare has no command descriptor", ce.Message)
	assert.Equal(t, 1, ce.ExitCode())

	var errOut bytes.Buffer
	root := newRoot()
	root.Writer, root.ErrWriter = &bytes.Buffer{}, &errOut
	err := Run(t.Context(), root, []string{"rancher", "bare"})
	assert.Equal(t, 1, exitCodeOf(err))
	assert.Contains(t, errOut.String(), "rancher bare has no command descriptor")
}
