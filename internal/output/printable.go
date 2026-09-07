package output

import (
	"strings"
	"unicode"
)

// Printable is the terminal-safety boundary for server-sourced text. It
// strips control characters (including the ANSI escape introducer, newline,
// tab, and DEL) and Unicode format characters (category Cf: bidi
// overrides, zero-width marks), so a string that originates from IdP
// claims, condition messages, object descriptions, or any other server
// data can neither drive the terminal nor forge, reorder, or hide what a
// text format shows. The table, wide, and name renderers apply it to every
// cell; text builders apply it at each insertion of a server field; the
// runtime applies it to warning and error lines. Structured formats (json,
// yaml, template) never pass through it - they return the bytes verbatim.
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}
