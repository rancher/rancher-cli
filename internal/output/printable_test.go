package output

import (
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
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
		{"a raw 0x9b byte, which is not valid UTF-8", "a\x9bb", "a\ufffdb"},
		{"an invalid UTF-8 byte", "abc\xff", "abc\ufffd"},
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

// foreignMarker is a marker of the form this process writes, from another
// process: U+2063 followed by 32 runes from U+2060, U+2061, U+2062 and U+2064.
var foreignMarker = "\u2063" + strings.Repeat("\u2060\u2061\u2062\u2064", 8)

// TestPrintableLinesKeepsNewlinesAndThisProcessesMarker asserts the filter
// for a whole text: the newlines between lines and every copy of this
// process's marker followed by the word rancher are kept; a bare U+2063,
// another process's marker, a carriage return, a tab and the escape
// character are removed; and a carriage return before a newline leaves the
// newline.
func TestPrintableLinesKeepsNewlinesAndThisProcessesMarker(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"newlines between lines", "a\nb\n\nc\n", "a\nb\n\nc\n"},
		{"this process's marker", "run " + clierr.Rancher + " x\n" + clierr.Rancher + " y", "run " + clierr.Rancher + " x\n" + clierr.Rancher + " y"},
		{"a bare U+2063", "a\u2063rancher b", "arancher b"},
		{"another process's marker", "a" + foreignMarker + "rancher b", "arancher b"},
		{"carriage return", "a\rb", "ab"},
		{"carriage return and newline", "a\r\nb\r\n", "a\nb\n"},
		{"tab", "a\tb", "ab"},
		{"escape sequence", "a\x1b[2Jb\nc\u009b2J", "a[2Jb\nc2J"},
		{"line and paragraph separators", "a\u2028b\u2029c\nd", "abc\nd"},
		{"a marker beside removed characters", "\x1b" + clierr.Rancher + "\u202e x", clierr.Rancher + " x"},
		{"a text with nothing to remove", "web - a project (1/2)\nsecond line\n", "web - a project (1/2)\nsecond line\n"},
		{"a raw 0x9b byte, which is not valid UTF-8", "a\x9b2Jb", "a\ufffd2Jb"},
		{"an invalid UTF-8 byte", "abc\xff\n", "abc\ufffd\n"},
		{"a raw 0x9b byte beside a marker", "a" + clierr.Rancher + "\x9bb", "a" + clierr.Rancher + "\ufffdb"},
		{"an empty text", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, PrintableLines(tc.in))
		})
	}
}

// TestPrintableLinesEqualsPrintableForEachLineOfATextWithNoMarker asserts
// that, for a text without this process's marker, the result is Printable
// of each line with the newlines put back.
func TestPrintableLinesEqualsPrintableForEachLineOfATextWithNoMarker(t *testing.T) {
	t.Parallel()
	text := "one\x1b[31m\ntwo\u202e\r\nthree\t\n\nfour\u2063x\u2028"
	var want []string
	for line := range strings.SplitSeq(text, "\n") {
		want = append(want, Printable(line))
	}
	assert.Equal(t, strings.Join(want, "\n"), PrintableLines(text))
}

// TestPrintableLinesReturnsACleanTextAsItIs asserts a text it does not
// change comes back as it was, also one that holds this process's marker.
func TestPrintableLinesReturnsACleanTextAsItIs(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"plain\ntext\n", "run " + clierr.Rancher + " x\n"} {
		assert.Equal(t, in, PrintableLines(in))
	}
}

// BenchmarkPrintableLinesWithNothingToRemove measures the filter on a 1 MiB
// text it does not change.
func BenchmarkPrintableLinesWithNothingToRemove(b *testing.B) {
	line := "a plain line of text with no character to remove\n"
	text := strings.Repeat(line, (1<<20)/len(line))
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for b.Loop() {
		_ = PrintableLines(text)
	}
}

// BenchmarkPrintableLinesWithMarkers measures the filter on 1 MiB that holds
// a remedy marker on every line.
func BenchmarkPrintableLinesWithMarkers(b *testing.B) {
	line := "run " + clierr.Rancher + " auth status now\n"
	text := strings.Repeat(line, (1<<20)/len(line))
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	for b.Loop() {
		_ = PrintableLines(text)
	}
}
