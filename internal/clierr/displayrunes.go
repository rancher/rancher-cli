package clierr

import (
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
// text, a new server entry name may not hold them, and the user id in the
// recovery advice and in the warnings that name an entry's next login or a
// superseded token's other owner has them removed with WithoutUnsafeToDisplay.
//
// The source scan in test/conformance refuses any other test of these classes
// through the unicode package outside its allowlist, so every caller shares
// this one rule.
func IsUnsafeToDisplay(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
}

// WithoutUnsafeToDisplay is s with every rune IsUnsafeToDisplay reports
// removed. The recovery advice of config.AdviseRecord and the warnings that
// name an entry's next login or a superseded token's other owner pass a user
// id through it: the id then cannot carry a newline, a direction control or
// an invisible rune. An id that holds only such runes comes back empty.
// output.Printable is this function, applied to displayed text.
func WithoutUnsafeToDisplay(s string) string {
	return strings.Map(func(r rune) rune {
		if IsUnsafeToDisplay(r) {
			return -1
		}
		return r
	}, s)
}
