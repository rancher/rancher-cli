package output

import (
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// Printable is the terminal-safety boundary for server-sourced text. It
// removes the runes clierr.IsUnsafeToDisplay reports: control characters
// (U+0000-U+001F, including the ANSI escape introducer, newline and tab;
// DEL; and U+0080-U+009F, including the control sequence introducer
// U+009B), Unicode format characters (category Cf: bidi overrides,
// zero-width marks, the invisible separator) and the line and paragraph
// separators U+2028 and U+2029, so a string that originates from IdP
// claims, condition messages, object descriptions, or any other server
// data can neither drive the terminal nor forge, reorder, or hide what a
// text format shows. The table, wide, and name renderers apply it to every
// cell; text builders apply it at each insertion of a server field; the
// runtime applies it to warning lines, to the message, hint and candidates of
// an error and to the preview fields. The details lines of an error show each
// value as clierr.IDText does, with these runes as escapes and nothing
// removed, so two values that differ by a hidden character print differently;
// Printable then only writes each invalid byte of such a line as U+FFFD.
// Structured formats (json, yaml, template) never pass through it - they
// return the bytes verbatim. A whole text with its own lines goes through
// PrintableLines, which the runtime applies to every text print and
// preview and internal/ui to every notice and prompt label.
//
// A byte that is not valid UTF-8, the control sequence introducer 0x9B
// included, is written as U+FFFD REPLACEMENT CHARACTER, which is printable
// and stays: strings.Map writes it for each invalid byte, so no byte a
// terminal in an 8-bit mode reads as a control reaches the terminal.
func Printable(s string) string {
	return clierr.WithoutUnsafeToDisplay(s)
}

// PrintableLines applies Printable to each line of s. It keeps every "\n"
// between lines and every copy of this process's remedy marker followed by
// the word rancher (clierr.Rancher), which the invocation replaces with the
// command it names before anything is written. Every other character
// Printable removes is removed, a bare U+2063 INVISIBLE SEPARATOR and
// another process's marker included, and "\r\n" becomes "\n". A text it
// does not change is returned as it is. An invalid UTF-8 byte becomes U+FFFD
// REPLACEMENT CHARACTER, as it does in Printable.
//
// Joining the pieces cannot form a marker: the pieces between two kept
// markers hold no format character after the filter, and a marker begins
// with U+2063.
func PrintableLines(s string) string {
	if !strings.Contains(s, clierr.Rancher) {
		return printableKeepingNewlines(s)
	}
	parts := strings.Split(s, clierr.Rancher)
	changed := false
	for i, p := range parts {
		if q := printableKeepingNewlines(p); q != p {
			parts[i] = q
			changed = true
		}
	}
	if !changed {
		return s
	}
	return strings.Join(parts, clierr.Rancher)
}

// printableKeepingNewlines is Printable for a text whose "\n" separate its
// lines.
func printableKeepingNewlines(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && clierr.IsUnsafeToDisplay(r) {
			return -1
		}
		return r
	}, s)
}
