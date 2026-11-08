package clierr

import (
	"fmt"
	"strconv"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
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
// escapes (never empty), a remedy marker start is shown as an escape, and an
// invalid UTF-8 byte with no hidden rune is unchanged.
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
		{"an invalid byte and no hidden rune", "u-a\xffb", "u-a\xffb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, IDText(tc.in))
		})
	}
}
