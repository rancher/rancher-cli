package configcmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestConfigEditRefusalWritesTheLeftOutNoteOnceForEntryNamesThatCannotBePrinted
// asserts that an edit adding two entries whose names hold a single quote is
// refused with no add-server command and the note once, though each refused
// entry left its own command out. Mutation: join the offenses without
// clierr.NoteOnce.
func TestConfigEditRefusalWritesTheLeftOutNoteOnceForEntryNamesThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	edited := &config.Config{Servers: map[string]config.Server{
		"it's":  {URL: "https://a.example.com"},
		"o'neo": {URL: "https://b.example.com"},
	}}
	err := checkEditRestrictions(nil, false, edited)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Message, "servers.it's added")
	assert.Contains(t, ce.Message, "servers.o'neo added")
	assert.NotContains(t, ce.Message, "add-server")
	assert.Equal(t, 1, strings.Count(ce.Message, clierr.LeftOutNote), ce.Message)
	assert.True(t, strings.HasSuffix(ce.Message, clierr.LeftOutNote), ce.Message)
}
