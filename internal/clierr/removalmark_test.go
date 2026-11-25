package clierr

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRemovalMarkIsFormatCharactersAndNeverARemedyMarker asserts the mark is
// U+2063, 16 characters of markerRunes, U+2063 and 16 more, that every
// character is one IsUnsafeToDisplay matches, that the mark has no remedy
// marker and a remedy marker has no mark, and that the mark next to a remedy
// command does not form a second remedy marker.
func TestRemovalMarkIsFormatCharactersAndNeverARemedyMarker(t *testing.T) {
	t.Parallel()
	mark := []rune(RemovalMark)
	assert.Len(t, mark, 34)
	for i, r := range mark {
		assert.True(t, IsUnsafeToDisplay(r))
		if i == 0 || i == 17 {
			assert.Equal(t, '\u2063', r, "character %d", i)
			continue
		}
		assert.Contains(t, markerRunes, r, "character %d", i)
	}
	assert.NotContains(t, RemovalMark, RemedyMarker)
	assert.NotContains(t, RemedyMarker, RemovalMark)
	assert.Equal(t, 1, strings.Count(RemovalMark+Rancher+" x", Rancher))
	assert.Equal(t, 1, strings.Count(Rancher+RemovalMark+" x", Rancher))
}

// TestRemovalMarkFunctions asserts WithoutRemovalMark removes every copy of
// the mark and nothing else, and HasRemovalMark finds it.
func TestRemovalMarkFunctions(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "ab", WithoutRemovalMark("a"+RemovalMark+"b"+RemovalMark))
	assert.Equal(t, "a\u2063b", WithoutRemovalMark("a\u2063b"))
	assert.Equal(t, Rancher+" x", WithoutRemovalMark(Rancher+RemovalMark+" x"))
	assert.True(t, HasRemovalMark("a"+RemovalMark))
	assert.False(t, HasRemovalMark("a\u2063\u2063b"))
}

// TestEscapeFunctionsRemoveTheRemovalMark asserts each of the four escape
// functions removes the mark before it escapes, with one row per function
// whose input gives another output unless the mark is removed: IDText escapes
// the value alone;
// LineText finds the clause boundary the mark hid; ValueText finds a remedy
// marker the mark split; WholeValueText quotes again a value that is a quoted
// Go string followed by the mark.
func TestEscapeFunctionsRemoveTheRemovalMark(t *testing.T) {
	t.Parallel()
	quoted := `"a\u200bb"`
	split := RemedyMarker[:3] + RemovalMark + RemedyMarker[3:] + "rancher get x"
	for _, tc := range []struct {
		name, got, want string
	}{
		{"IDText", IDText("a\u200bb" + RemovalMark), `"a\u200bb"`},
		{"IDText with nothing else to escape", IDText("ab" + RemovalMark), "ab"},
		{"LineText", LineText("x\u200b;" + RemovalMark + " y"), `"x\u200b"; y`},
		{"ValueText", ValueText(split), Rancher + " get x"},
		{"WholeValueText", WholeValueText(quoted + RemovalMark), strconv.Quote(quoted)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.got)
			assert.NotContains(t, tc.got, RemovalMark, "no mark is left")
		})
	}
}

// TestErrorWithoutRemovalMark asserts the error copy has the mark removed
// from its message, hint and warnings, keeps the rest, and that an error
// with no mark comes back as it is.
func TestErrorWithoutRemovalMark(t *testing.T) {
	t.Parallel()
	e := New(CodeNotFound, "m"+RemovalMark).WithHint("h"+RemovalMark).WithWarnings("w"+RemovalMark, "v")
	got := e.WithoutRemovalMark()
	assert.Equal(t, "m", got.Message)
	assert.Equal(t, "h", got.Hint)
	assert.Equal(t, []string{"w", "v"}, got.Warnings())
	assert.Equal(t, CodeNotFound, got.Code)
	assert.Equal(t, "m"+RemovalMark, e.Message, "the original keeps its text")
	clean := New(CodeNotFound, "m")
	assert.Same(t, clean, clean.WithoutRemovalMark())
}
