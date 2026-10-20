package output

import (
	"strings"
	"unicode"
)

// Printable is the terminal-safety boundary for server-sourced text. It
// strips control characters (U+0000-U+001F, including the ANSI escape
// introducer, newline and tab; DEL; and U+0080-U+009F, including the
// one-byte sequence introducer U+009B), Unicode format characters
// (category Cf: bidi overrides, zero-width marks, the invisible
// separator) and the line and paragraph
// separators U+2028 and U+2029, so a string that originates from IdP
// claims, condition messages, object descriptions, or any other server
// data can neither drive the terminal nor forge, reorder, or hide what a
// text format shows. The table, wide, and name renderers apply it to every
// cell; text builders apply it at each insertion of a server field; the
// runtime applies it to warning and error lines. Structured formats (json,
// yaml, template) never pass through it - they return the bytes verbatim.
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, s)
}
