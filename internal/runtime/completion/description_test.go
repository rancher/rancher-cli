package completion

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

// TestSanitizeDescription asserts every rule of the sanitizer: control,
// private-use, unassigned, separator and replacement runes and invalid
// UTF-8 become a space; format runes are deleted; runs of space collapse
// and the ends are trimmed; text with no visible rune has no description;
// a longer text is cut at a rune boundary, trimmed and ends in "...".
func TestSanitizeDescription(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain text", in: "Alice Smith", want: "Alice Smith"},
		{name: "empty", in: "", want: ""},
		{name: "newline", in: "a\nb", want: "a b"},
		{name: "tab", in: "a\tb", want: "a b"},
		{name: "carriage return", in: "a\rb", want: "a b"},
		{name: "escape sequence", in: "x\x1b[31m", want: "x [31m"},
		{name: "bell", in: "a\x07b", want: "a b"},
		{name: "NUL", in: "a\x00b", want: "a b"},
		{name: "next line control", in: "a\u0085b", want: "a b"},
		{name: "line separator", in: "a\u2028b", want: "a b"},
		{name: "paragraph separator", in: "a\u2029b", want: "a b"},
		{name: "right-to-left override is deleted", in: "a\u202eb", want: "ab"},
		{name: "zero width space is deleted", in: "a\u200bb", want: "ab"},
		{name: "byte order mark is deleted", in: "a\ufeffb", want: "ab"},
		{name: "mongolian vowel separator is deleted", in: "a\u180eb", want: "ab"},
		{name: "soft hyphen is deleted", in: "a\u00adb", want: "ab"},
		{name: "zero width joiner is deleted", in: "a\u200db", want: "ab"},
		{name: "replacement character", in: "a\ufffdb", want: "a b"},
		{name: "invalid UTF-8", in: "a\xffb", want: "a b"},
		{name: "private use", in: "a\ue000b", want: "a b"},
		{name: "unassigned", in: "a\u0378b", want: "a b"},
		{name: "no-break space only", in: "\u00a0", want: ""},
		{name: "hangul filler only", in: "\u3164", want: ""},
		{name: "hangul jungseong filler only", in: "\u1160", want: ""},
		{name: "blank braille only", in: "\u2800", want: ""},
		{name: "fillers and spaces only", in: " \u3164 \u2800\t", want: ""},
		{name: "only control runes", in: "\x1b\x07\x00", want: ""},
		{name: "only format runes", in: "\u200b\u202e", want: ""},
		{name: "a filler beside a visible rune stays", in: "x\u3164", want: "x\u3164"},
		{name: "run of spaces", in: "a   b", want: "a b"},
		{name: "mixed space runes", in: "a \u00a0\t\n b", want: "a b"},
		{name: "no-break spaces collapse", in: "a\u00a0\u00a0b", want: "a b"},
		{name: "ends trimmed", in: "  a b  ", want: "a b"},
		{name: "non-ASCII text", in: "Zo\u00eb M\u00fcller \u65e5\u672c\u8a9e", want: "Zo\u00eb M\u00fcller \u65e5\u672c\u8a9e"},
		{name: "colon kept", in: "dev: build team", want: "dev: build team"},
		{name: "56 runes unchanged", in: strings.Repeat("a", 56), want: strings.Repeat("a", 56)},
		{name: "57 runes cut", in: strings.Repeat("a", 57), want: strings.Repeat("a", 56) + "..."},
		{name: "56 multibyte runes unchanged", in: strings.Repeat("\u00e9", 56), want: strings.Repeat("\u00e9", 56)},
		{name: "a multibyte rune at the cut", in: strings.Repeat("a", 55) + "\u00e9\u00e9", want: strings.Repeat("a", 55) + "\u00e9..."},
		{name: "a space at the cut is trimmed", in: strings.Repeat("a", 55) + " bcd", want: strings.Repeat("a", 55) + "..."},
		{name: "the cut counts runes after the sanitizer", in: strings.Repeat("a", 40) + strings.Repeat("\u200b", 30) + strings.Repeat("b", 20), want: strings.Repeat("a", 40) + strings.Repeat("b", 16) + "..."},
		{name: "a cut that leaves only fillers has no description", in: strings.Repeat("\u3164", 56) + "x", want: ""},
		{name: "long text of control runes only", in: strings.Repeat("\n", 100), want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := SanitizeDescription(tt.in)
			assert.Equal(t, tt.want, got)
			assert.True(t, utf8.ValidString(got), "the result is valid UTF-8")
			assert.LessOrEqual(t, utf8.RuneCountInString(got), MaxDescriptionRunes+len("..."))
			assert.NotContains(t, got, "\n")
		})
	}
}

// TestItemString asserts the reader follows the path through objects and
// answers "" for a missing step, a value that is not a string and an
// empty path.
func TestItemString(t *testing.T) {
	t.Parallel()
	item := map[string]any{
		"displayName": "Top",
		"spec":        map[string]any{"displayName": "Nested", "count": 3.0, "inner": map[string]any{"x": "deep"}},
		"list":        []any{"a"},
	}
	assert.Equal(t, "Top", ItemString(item, []string{"displayName"}))
	assert.Equal(t, "Nested", ItemString(item, []string{"spec", "displayName"}))
	assert.Equal(t, "deep", ItemString(item, []string{"spec", "inner", "x"}))
	assert.Empty(t, ItemString(item, []string{"missing"}))
	assert.Empty(t, ItemString(item, []string{"spec", "missing"}))
	assert.Empty(t, ItemString(item, []string{"spec", "count"}), "a number is not a string")
	assert.Empty(t, ItemString(item, []string{"spec"}), "an object is not a string")
	assert.Empty(t, ItemString(item, []string{"list", "0"}), "a list is not walked")
	assert.Empty(t, ItemString(item, []string{"displayName", "x"}), "a string has no field")
	assert.Empty(t, ItemString(item, nil))
	assert.Empty(t, ItemString(nil, []string{"x"}))
}

// TestJoinDescription asserts the first non-empty value leads, the others
// that differ follow once in parentheses and comma-separated, and a value
// with no visible rune counts as empty.
func TestJoinDescription(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "alice (Alice Smith)", JoinDescription("alice", "Alice Smith"))
	assert.Equal(t, "alice", JoinDescription("alice", ""))
	assert.Equal(t, "Alice Smith", JoinDescription("", "Alice Smith"))
	assert.Equal(t, "alice", JoinDescription("alice", "alice"))
	assert.Equal(t, "alice", JoinDescription("alice", "  "))
	assert.Equal(t, "zw", JoinDescription("zw", "\u200b"), "a display name of a format rune only is no display name")
	assert.Equal(t, "zw", JoinDescription("\u200b", "zw"))
	assert.Equal(t, "zw", JoinDescription("zw", "\u3164"))
	assert.Equal(t, "alice (a, b)", JoinDescription("alice", "a", "alice", "b", "a"))
	assert.Empty(t, JoinDescription())
	assert.Empty(t, JoinDescription("", " "))
	assert.Equal(t, "Top (Other)", JoinDescription(" Top ", "Other"))
}

// TestItemDescription asserts the description of an item reads the paths
// in order, nested paths included.
func TestItemDescription(t *testing.T) {
	t.Parallel()
	item := map[string]any{"username": "alice", "displayName": "Alice Smith", "spec": map[string]any{"displayName": "Nested", "description": "ignored"}}
	assert.Equal(t, "alice (Alice Smith)", ItemDescription(item, [][]string{{"username"}, {"displayName"}}))
	assert.Equal(t, "Nested", ItemDescription(item, [][]string{{"spec", "displayName"}}))
	assert.Empty(t, ItemDescription(item, [][]string{{"nothing"}}))
	assert.Empty(t, ItemDescription(item, nil))
}
