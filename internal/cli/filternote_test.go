package cli

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noFilterNoteCommands are the commands whose descriptor sets NoFilterNote,
// each with the reason it prints no filter note.
var noFilterNoteCommands = map[string]string{
	"auth logout":          "its text has the lines of a certificate failure with their hidden characters removed",
	"auth delete-token":    "its text has the lines of a certificate failure with their hidden characters removed",
	"config delete-server": "its text has the lines of a certificate failure with their hidden characters removed",
	"config update-server": "its text has the lines of a certificate failure with their hidden characters removed",
}

// TestNoFilterNoteIsSetOnlyOnTheListedCommands asserts, over every command the
// root registers, the generated ones included, that the commands whose
// descriptor sets NoFilterNote are exactly the listed ones: a command outside
// the list that sets it fails, and so does a listed command that does not.
func TestNoFilterNoteIsSetOnlyOnTheListedCommands(t *testing.T) {
	t.Parallel()
	wr, err := buildWiredRegistry("v-test")
	require.NoError(t, err)
	require.Greater(t, len(wr.reg.Commands()), 150, "the registry has every command")
	var set []string
	for _, c := range wr.reg.Commands() {
		if d := c.Describe(); d.NoFilterNote {
			set = append(set, d.Name)
		}
	}
	for _, name := range set {
		_, listed := noFilterNoteCommands[name]
		assert.True(t, listed, "%s sets NoFilterNote and is not in the list of commands that may", name)
	}
	for name, why := range noFilterNoteCommands {
		assert.NotEmpty(t, why, "%s has no reason", name)
		assert.True(t, slices.Contains(set, name), "%s is in the list and does not set NoFilterNote", name)
	}
}
