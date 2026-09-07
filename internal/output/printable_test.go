package output

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPrintableStripsControlAndFormatCharacters asserts the boundary: C0
// controls (newline, tab, escape), DEL, and Unicode format characters
// (bidi overrides, zero-width joiners) are removed; letters, spaces,
// punctuation, and non-ASCII letters survive unchanged.
func TestPrintableStripsControlAndFormatCharacters(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"escape sequence introducer", "a\x1b[2Jb", "a[2Jb"},
		{"newline and tab", "line1\nline2\tx", "line1line2x"},
		{"carriage return and delete", "a\rb\x7fc", "abc"},
		{"bidi override and zero-width joiner", "a\u202eb\u200dc", "abc"},
		{"plain text survives", "web - a project (1/2)", "web - a project (1/2)"},
		{"non-ascii letters survive", "caf\u00e9", "caf\u00e9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, Printable(tc.in))
		})
	}
}
