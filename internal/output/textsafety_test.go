package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRenderTextFormatsStripControlCharacters asserts the table, wide, and
// name renderers strip control characters from every cell and header, so a
// server-sourced value with an embedded newline or escape sequence cannot
// forge a row or drive the terminal.
func TestRenderTextFormatsStripControlCharacters(t *testing.T) {
	env := NewEnvelope("ProjectList", namedRows{rows: [][]string{{"p-1\x1b[31m", "line1\nFAKE ROW\x1b[2J"}}})
	for _, format := range []string{"table", "wide"} {
		var buf bytes.Buffer
		require.NoError(t, Render(&buf, format, env))
		out := buf.String()
		assert.NotContains(t, out, "\x1b")
		assert.Len(t, strings.Split(strings.TrimRight(out, "\n"), "\n"), 2, "header and one row, no forged row: %q", out)
		assert.Contains(t, out, "line1FAKE ROW[2J")
	}
	var buf bytes.Buffer
	require.NoError(t, Render(&buf, "name", env))
	assert.Equal(t, "p-1[31m\n", buf.String())
}
