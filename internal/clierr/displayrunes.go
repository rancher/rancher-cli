package clierr

import (
	"strconv"
	"strings"
	"unicode"
)

// IsUnsafeToDisplay reports whether the CLI never shows r raw. It is true for
// three classes, in this order:
//   - Control characters (unicode.IsControl: U+0000-U+001F, U+007F and
//     U+0080-U+009F). ESC and U+009B start terminal control sequences;
//     newline, carriage return, U+0085 and tab end or rewrite a line, so a
//     value can forge a line or a row.
//   - Invisible format characters (Unicode category Cf). Bidi embeddings,
//     overrides, isolates and marks reorder the text around them, so a value
//     can display as another value. Zero-width characters, the word joiner,
//     the invisible operators, U+FEFF and the soft hyphen are invisible, so
//     two different values print the same: a rune that hides a difference
//     between two values counts as unsafe even when it does nothing else.
//     U+2063 also opens the remedy marker.
//   - The line separator U+2028 and the paragraph separator U+2029, where some
//     terminals and editors break the line.
//
// No other rune is covered. A byte that is not valid UTF-8 is not a rune and
// is handled by each caller.
//
// The callers' rule: output.Printable removes these runes from displayed
// text, a new server entry name may not hold them, and a user id the CLI did
// not validate goes into error, hint, warning and recovery text through
// IDText, which shows the id with these runes as escapes.
//
// The source scan in test/conformance refuses any other test of these classes
// through the unicode package outside its allowlist, so every caller shares
// this one rule.
func IsUnsafeToDisplay(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
}

// WithoutUnsafeToDisplay is s with every rune IsUnsafeToDisplay reports
// removed. Its only caller is output.Printable, which applies it to
// displayed text. A user id never goes through it: removing runes could turn
// one id into another real id, so IDText writes them as escapes. An input
// that holds only such runes comes back empty.
func WithoutUnsafeToDisplay(s string) string {
	return strings.Map(func(r rune) rune {
		if IsUnsafeToDisplay(r) {
			return -1
		}
		return r
	}, s)
}

// IDText is id as it goes into error, hint, warning and recovery text. An id
// none of whose runes IsUnsafeToDisplay reports is returned unchanged. Any
// other id is returned as a double-quoted Go string with those runes written
// as escapes, so the text shows the exact id and no rune hides or reorders the
// text around it. The runes are never removed: removing them could turn one id
// into another real id. An invalid UTF-8 byte is not a rune IsUnsafeToDisplay
// matches, so an id with only such a byte comes back unchanged.
func IDText(id string) string {
	if strings.IndexFunc(id, IsUnsafeToDisplay) < 0 {
		return id
	}
	return strconv.Quote(id)
}
