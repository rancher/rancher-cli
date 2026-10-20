package output

import (
	"testing"
	"unicode"

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
		{"one-byte control sequence introducer", "a\u009b2Jb", "a2Jb"},
		{"line and paragraph separators", "a\u2028b\u2029c", "abc"},
		{"invisible separator", "a\u2063b", "ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, Printable(tc.in))
		})
	}
}

// TestPrintableDropsExactlyControlFormatAndSeparatorRunes asserts, for
// every code point, that Printable drops it when it is a control rune
// (U+0000-U+001F, DEL and U+0080-U+009F), a format rune (Cf) or the
// line or paragraph separator (U+2028, U+2029), and keeps it otherwise.
func TestPrintableDropsExactlyControlFormatAndSeparatorRunes(t *testing.T) {
	t.Parallel()
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue // surrogates are not valid in a Go string
		}
		dropped := r <= 0x1f || (r >= 0x7f && r <= 0x9f) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029
		in := "a" + string(r) + "b"
		want := in
		if dropped {
			want = "ab"
		}
		if got := Printable(in); got != want {
			t.Fatalf("Printable(%q) = %q, want %q (U+%04X)", in, got, want, r)
		}
	}
}
