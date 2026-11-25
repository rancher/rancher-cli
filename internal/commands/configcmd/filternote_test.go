package configcmd

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/stretchr/testify/assert"
)

// TestServerDeletesPrintNoFilterNote asserts config delete-server and config
// update-server, whose text has the lines of a certificate failure with
// their hidden characters removed, print no line about removed characters.
func TestServerDeletesPrintNoFilterNote(t *testing.T) {
	t.Parallel()
	assert.True(t, configDeleteServerCmd{}.Describe().NoFilterNote)
	assert.True(t, configUpdateServerCmd{}.Describe().NoFilterNote)
}

// TestProjectFieldsKeepsOneKeyForTwoFilteredKeys asserts that two keys that
// differ only by a character the filter removed are one key of the json
// projection, the later value winning, with no mark in a key or a value, and
// that the text keeps both lines and the mark of the changed key.
func TestProjectFieldsKeepsOneKeyForTwoFilteredKeys(t *testing.T) {
	t.Parallel()
	fields := []field{{output.FilterValue("ab"), clierr.Plain("first")}, {output.FilterValue("a\u200bb"), output.FilterValue("s\u200bx")}}
	assert.Equal(t, map[string]any{"ab": "sx"}, projectFields(fields))
	var text clierr.Builder
	renderFields(fields, "", &text)
	got := text.Filtered().Marked()
	assert.True(t, clierr.HasRemovalMark(got), "the text has the mark of each changed value")
	assert.Equal(t, "ab: first\nab: sx\n", output.PrintableLines(got))
}
