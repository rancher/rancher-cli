package clierr

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writtenOutDisplayRule is the set of runes never shown raw, written out
// class by class.
func writtenOutDisplayRule(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029'
}

// TestIsUnsafeToDisplayMatchesTheWrittenOutRule asserts, for every valid code
// point, that IsUnsafeToDisplay equals the rule written out class by class:
// control, format, and the line and paragraph separators. It also asserts
// that every control rune is unsafe.
func TestIsUnsafeToDisplayMatchesTheWrittenOutRule(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		assert.Equal(t, writtenOutDisplayRule(r), IsUnsafeToDisplay(r), "U+%04X", r)
		if unicode.IsControl(r) {
			assert.True(t, IsUnsafeToDisplay(r), "control U+%04X", r)
		}
	}
}

// TestWithoutUnsafeToDisplayAgreesWithIsUnsafeToDisplay asserts, for every
// valid code point inside a string and for a table of strings, that the
// result holds no rune IsUnsafeToDisplay reports, that a string with no such
// rune comes back unchanged, and that a rune it reports is the only thing
// removed.
func TestWithoutUnsafeToDisplayAgreesWithIsUnsafeToDisplay(t *testing.T) {
	var wrong []string
	for r := rune(0); r <= unicode.MaxRune && len(wrong) < 20; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		s := "a" + string(r) + "b"
		got := WithoutUnsafeToDisplay(s)
		want := s
		if IsUnsafeToDisplay(r) {
			want = "ab"
		}
		if got != want {
			wrong = append(wrong, fmt.Sprintf("U+%04X: got %q, want %q", r, got, want))
		}
	}
	assert.Empty(t, wrong)

	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"u-abc12", "u-abc12"},
		{"u-a\u202eb\u2028c\u2029d", "u-abcd"},
		{"u-a\n$(id)\x1bbc", "u-a$(id)bc"},
		{"\u202e\u2063", ""},
		{"\u2028\u2029\u200b\ufeff\u00ad", ""},
		{"\u4e16\u754c \u00e9 \u3164 \ufffd", "\u4e16\u754c \u00e9 \u3164 \ufffd"},
	} {
		got := WithoutUnsafeToDisplay(tc.in)
		assert.Equal(t, tc.want, got, "%q", tc.in)
		for _, r := range got {
			assert.False(t, IsUnsafeToDisplay(r), "%q holds U+%04X", got, r)
		}
	}
}

// TestIDTextEscapesAnIdWithAHiddenRuneAndKeepsEveryOther asserts, for every
// valid code point inside an id, that IDText returns the id unchanged when the
// rune is not one IsUnsafeToDisplay reports and the id as a double-quoted Go
// string otherwise, and that the quoted form holds no such rune and reads back
// as the exact id: nothing is removed.
func TestIDTextEscapesAnIdWithAHiddenRuneAndKeepsEveryOther(t *testing.T) {
	var wrong []string
	for r := rune(0); r <= unicode.MaxRune && len(wrong) < 20; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		id := "u-a" + string(r) + "b"
		got := IDText(id)
		if !IsUnsafeToDisplay(r) {
			if got != id {
				wrong = append(wrong, fmt.Sprintf("U+%04X changed: %q", r, got))
			}
			continue
		}
		unquoted, err := strconv.Unquote(got)
		if err != nil || unquoted != id || got != strconv.Quote(id) || WithoutUnsafeToDisplay(got) != got {
			wrong = append(wrong, fmt.Sprintf("U+%04X: %q", r, got))
		}
	}
	assert.Empty(t, wrong)
}

// TestIDTextRows asserts the rows the callers rely on: a plain id and an
// email-style name are unchanged, an id of only hidden runes is shown as
// escapes (never empty), a remedy marker start is shown as an escape, an
// invalid UTF-8 byte is shown as an escape, and a value with a single quote is
// quoted only in the pieces between the quotes that need it.
func TestIDTextRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"a user id", "u-abc12", "u-abc12"},
		{"an email name", "alice@example.com", "alice@example.com"},
		{"a name with a space", "a b", "a b"},
		{"an empty id", "", ""},
		{"only hidden runes", "\u202e\u2063", `"\u202e\u2063"`},
		{"a zero width space inside", "u-abc\u200b12", `"u-abc\u200b12"`},
		{"a newline", "u-a\nb", `"u-a\nb"`},
		{"a bell", "u-a\ab", `"u-a\ab"`},
		{"the start of a remedy marker", "u\u2063rancher", `"u\u2063rancher"`},
		{"an invalid byte and no hidden rune", "u-a\xffb", `"u-a\xffb"`},
		{"a single quote and no hidden rune", "o'b", "o'b"},
		{"a single quote and a hidden rune", "o'b\u200b", `o'"b\u200b"`},
		{"a hidden rune between single quotes", "'\u200b'", `'"\u200b"'`},
		{"a single quote and an invalid byte", "a'\xffb", `a'"\xffb"`},
		{"a printable non-ASCII rune beside a hidden rune", "caf\u00e9\u200b", "\"caf\u00e9\\u200b\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, IDText(tc.in))
		})
	}
}

type idTextCase struct {
	name string
	in   string
	// notPrintable is NotPrintable of in, written out.
	notPrintable bool
}

// idTextCases is every input of the IDText and NotPrintable tables.
func idTextCases() []idTextCase {
	around := func(r rune) string { return "u-a" + string(r) + "b" }
	var cases []idTextCase
	for _, r := range []rune{0x0, 0xa, 0xd, 0x1f, 0x7f, 0x9b, 0x200b, 0x202e, 0xfeff, 0xad, 0x2063, 0x2028, 0x2029} {
		cases = append(cases, idTextCase{fmt.Sprintf("U+%04X", r), around(r), true})
	}
	cases = append(cases, idTextCase{"the start of a remedy marker", "u\u2063rancher", true})
	for b := 0x80; b <= 0xff; b++ {
		cases = append(cases, idTextCase{fmt.Sprintf("byte 0x%02x", b), "u-a" + string([]byte{byte(b)}) + "b", true})
	}
	return append(cases,
		idTextCase{"a truncated sequence", "\xe2\x80", true},
		idTextCase{"a single quote", "o'b", false},
		idTextCase{"only a single quote", "'", false},
		idTextCase{"two single quotes", "''", false},
		idTextCase{"a quoted single quote", `'\''`, false},
		idTextCase{"a single quote and a hidden rune", "o'b\u200b", true},
		idTextCase{"a hidden rune between single quotes", "'\u200b'", true},
		idTextCase{"a hidden rune around a single quote", "\u200b'\u200b", true},
		idTextCase{"a single quote and an invalid byte", "a'\xffb", true},
		idTextCase{"a printable non-ASCII rune beside a hidden rune", "caf\u00e9\u200b", true},
		idTextCase{"a printable non-ASCII rune beside an invalid byte", "caf\u00e9\xff", true},
		idTextCase{"a printable non-ASCII rune and a single quote beside a hidden rune", "caf\u00e9'\u200b", true},
		idTextCase{"a double quoted word in single quotes", `'"ops"'`, false},
		idTextCase{"a variable", "$PASSWORD", false},
		idTextCase{"a space", "a b", false},
		idTextCase{"a clause separator", "; ", false},
		idTextCase{"an open parenthesis", " (", false},
		idTextCase{"the empty string", "", false},
	)
}

// quotedSegment reports whether s is exactly the double-quoted form
// strconv.Quote writes for a text NotPrintable reports true for.
func quotedSegment(s string) bool {
	unquoted, err := strconv.Unquote(s)
	return err == nil && strconv.Quote(unquoted) == s && NotPrintable(unquoted)
}

// hasQuotedSegment reports whether the whole of s or a piece of s cut at a
// single quote is a quotedSegment.
func hasQuotedSegment(s string) bool {
	if quotedSegment(s) {
		return true
	}
	for piece := range strings.SplitSeq(s, "'") {
		if quotedSegment(piece) {
			return true
		}
	}
	return false
}

// TestIDTextEscapesEveryHiddenRuneAndInvalidByteAndNothingElse asserts, for
// each input of the table: the output holds no rune IsUnsafeToDisplay matches
// and no invalid byte; cutting the input and the output at each single quote
// gives the same number of pieces, each output piece being the input piece
// when NotPrintable is false for it and strconv.Quote of it otherwise (so no
// double-quoted piece holds a single quote); each quoted piece reads back as
// the input piece, so nothing is removed; NotPrintable of the input is true
// exactly when IDText changes it; and an input with no hidden rune and valid
// UTF-8 comes back unchanged.
func TestIDTextEscapesEveryHiddenRuneAndInvalidByteAndNothingElse(t *testing.T) {
	t.Parallel()
	for _, tc := range idTextCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := IDText(tc.in)
			assert.Less(t, strings.IndexFunc(got, IsUnsafeToDisplay), 0, "the output holds a rune the terminal hides: %q", got)
			assert.True(t, utf8.ValidString(got), "the output holds an invalid byte: %q", got)
			assert.Equal(t, tc.notPrintable, got != tc.in)
			assert.Equal(t, got != tc.in, NotPrintable(tc.in))
			if !tc.notPrintable {
				assert.Equal(t, tc.in, got)
			}
			in, out := strings.Split(tc.in, "'"), strings.Split(got, "'")
			require.Len(t, out, len(in), "%q is %q", tc.in, got)
			for i := range in {
				if !NotPrintable(in[i]) {
					assert.Equal(t, in[i], out[i], "piece %d of %q", i, tc.in)
					continue
				}
				assert.Equal(t, strconv.Quote(in[i]), out[i], "piece %d of %q", i, tc.in)
				unquoted, err := strconv.Unquote(out[i])
				require.NoError(t, err)
				assert.Equal(t, in[i], unquoted)
			}
		})
	}
}

// TestIDTextGivesTwoDifferentInputsTwoDifferentOutputs asserts, over every
// two different inputs of the table, that their outputs differ. A pair is
// exempt only when its outputs are equal and one input has a piece (the whole
// input, or a piece cut at a single quote) that strconv.Unquote accepts, whose
// unquoted text strconv.Quote writes back as that piece, and for which
// NotPrintable is true: a value that is itself a quoted Go string of a hidden
// text. A piece such as "ops" or "$PASSWORD" unquotes to a printable text and
// exempts nothing.
func TestIDTextGivesTwoDifferentInputsTwoDifferentOutputs(t *testing.T) {
	t.Parallel()
	cases := idTextCases()
	for i, a := range cases {
		for _, b := range cases[i+1:] {
			if a.in == b.in {
				continue
			}
			if IDText(a.in) != IDText(b.in) {
				continue
			}
			assert.True(t, hasQuotedSegment(a.in) || hasQuotedSegment(b.in), "%q and %q both print as %q", a.in, b.in, IDText(a.in))
		}
	}
}

// TestIDTextEscapesEveryInvalidByte asserts, for every byte 0x80-0xff inside
// an id, that IDText writes the id as a double-quoted Go string that reads back
// as the exact id, and that the text holds no invalid byte.
func TestIDTextEscapesEveryInvalidByte(t *testing.T) {
	var wrong []string
	for b := 0x80; b <= 0xff && len(wrong) < 20; b++ {
		id := "u-a" + string([]byte{byte(b)}) + "b"
		got := IDText(id)
		unquoted, err := strconv.Unquote(got)
		if err != nil || unquoted != id || got != strconv.Quote(id) || !utf8.ValidString(got) || !strings.Contains(got, fmt.Sprintf(`\x%02x`, b)) {
			wrong = append(wrong, fmt.Sprintf("0x%02x: %q", b, got))
		}
	}
	assert.Empty(t, wrong)
}

// TestNotPrintable asserts NotPrintable is true for a value that holds a rune
// IsUnsafeToDisplay matches, the start of the remedy marker included, or is
// not valid UTF-8, and false for every other value, a single quote included.
func TestNotPrintable(t *testing.T) {
	t.Parallel()
	for _, tc := range idTextCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.notPrintable, NotPrintable(tc.in))
		})
	}
}
