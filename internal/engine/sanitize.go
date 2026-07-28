package engine

import (
	"strings"
	"unicode"
)

// Printable strips control characters (including ANSI escape introducers)
// and Unicode format characters (category Cf: bidi overrides, zero-width
// marks) from a server-sourced string before it enters any text output —
// strings that originate from IdP claims, condition messages, or other
// server data must neither drive the terminal nor reorder/hide what it shows.
func Printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
}
