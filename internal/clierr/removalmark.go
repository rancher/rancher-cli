package clierr

import (
	"strings"
	"unicode/utf8"
)

// RemovalMark follows a value that a filter changed before the value went
// into a result text or a notice: a character a terminal cannot show was
// removed, an invalid UTF-8 byte was written as U+FFFD, or a newline was
// removed. The renderer of a result text or a notice that finds it writes one
// line that says the output had characters a terminal cannot show. It marks
// nothing else: a lost mark loses that line, and a stray one adds it.
//
// The mark is U+2063 (INVISIBLE SEPARATOR), 16 characters, U+2063 again and
// 16 more. The 32 characters are chosen for each process from markerRunes, as
// RemedyMarker's are. Text that came from a server, a file or another process
// cannot have the mark. It is not a remedy marker: a remedy marker is U+2063
// followed by 32 such characters in a row, and the second U+2063 of this mark
// stops the run at 16. All of its characters are Unicode format characters,
// which output.Printable removes. Every place that writes a value without
// announcing the change removes the mark before anything else: IDText,
// LineText, ValueText and WholeValueText, the writer of a result's or a
// preview's output in internal/runtime, and the error renderer
// (Error.WithoutRemovalMark).
var RemovalMark = removalMarkOf(markerFromBytes(markerBytes()))

// removalMarkOf returns m, a marker from markerFromBytes, with its first
// character repeated after the 16 characters that follow it.
func removalMarkOf(m string) string {
	first, size := utf8.DecodeRuneInString(m)
	half := size + 16*utf8.RuneLen(markerRunes[0])
	return m[:half] + string(first) + m[half:]
}

// WithoutRemovalMark is s with every copy of RemovalMark removed. A width
// measure calls it to count only the characters a terminal shows, and each
// place that writes s without announcing a change calls it first.
func WithoutRemovalMark(s string) string {
	if !strings.Contains(s, RemovalMark) {
		return s
	}
	return strings.ReplaceAll(s, RemovalMark, "")
}

// HasRemovalMark reports whether s has RemovalMark: a filter changed a
// value in s.
func HasRemovalMark(s string) bool { return strings.Contains(s, RemovalMark) }

// WithoutRemovalMark returns e with RemovalMark removed from its message, its
// hint and each of its warnings. e itself is returned when none of them has
// the mark. A relayed result text or notice can carry the mark into an error;
// the error says nothing about a removed character, and its json and yaml
// carry the text without the mark. Details values and the name and id of each
// candidate lose the mark in WholeValueText when they are printed; no filtered
// value reaches the json of the details or of the candidates.
func (e *Error) WithoutRemovalMark() *Error {
	has := HasRemovalMark(e.Message) || HasRemovalMark(e.Hint)
	for _, w := range e.warnings {
		has = has || HasRemovalMark(w)
	}
	if !has {
		return e
	}
	c := *e
	c.Message = WithoutRemovalMark(e.Message)
	c.Hint = WithoutRemovalMark(e.Hint)
	c.warnings = make([]string, len(e.warnings))
	for i, w := range e.warnings {
		c.warnings[i] = WithoutRemovalMark(w)
	}
	return &c
}
