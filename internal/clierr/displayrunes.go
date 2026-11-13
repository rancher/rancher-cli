package clierr

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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
// No other rune is covered. A byte that is not valid UTF-8 is not a rune;
// NotPrintable covers it with these runes.
//
// The callers' rule: output.Printable removes these runes from displayed
// text, a new server entry name may not hold them, a user id the CLI did
// not validate goes into error, hint, warning and recovery text through
// IDText, which shows the id with these runes and any invalid byte as
// escapes, and each value in the details lines of an error is shown the same
// way by the renderer.
//
// The source scan in test/conformance refuses any other test of these classes
// through the unicode package outside its allowlist, so every caller shares
// this one rule.
func IsUnsafeToDisplay(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
}

// WithoutUnsafeToDisplay is s with every rune IsUnsafeToDisplay reports
// removed. output.Printable applies it to displayed text. A user id and a
// value in the details lines of an error never go through it: removing runes
// could turn one value into another real value, so IDText writes them as
// escapes. An input that holds only such runes comes back empty.
func WithoutUnsafeToDisplay(s string) string {
	return strings.Map(func(r rune) rune {
		if IsUnsafeToDisplay(r) {
			return -1
		}
		return r
	}, s)
}

// NotPrintable reports whether v holds a rune IsUnsafeToDisplay matches, the
// start of the remedy marker (U+2063) included, or is not valid UTF-8. It is
// the same test as IDText(v) != v.
func NotPrintable(v string) bool {
	return !utf8.ValidString(v) || strings.IndexFunc(v, IsUnsafeToDisplay) >= 0
}

// IDText is id as it goes into error, hint, warning and recovery text. An id
// that NotPrintable reports false for is returned unchanged. Any other id is
// cut at each single quote; each piece that NotPrintable reports true for is
// written as a double-quoted Go string, with each rune IsUnsafeToDisplay
// matches and each invalid UTF-8 byte as escapes, and every single quote and
// every other piece stays as written. The text shows the exact id and no rune
// hides or reorders the text around it. No double-quoted piece holds a single
// quote, and ShellQuote of the result never puts a single quote inside double
// quotes. Nothing is removed:
// removing a rune or replacing a byte could turn one id into another real id.
func IDText(id string) string {
	if !NotPrintable(id) {
		return id
	}
	pieces := strings.Split(id, "'")
	for i, piece := range pieces {
		if NotPrintable(piece) {
			pieces[i] = strconv.Quote(piece)
		}
	}
	return strings.Join(pieces, "'")
}
