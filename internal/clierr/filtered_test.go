package clierr

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFilteredKeepsItsText asserts that every way to build a Filtered keeps
// the text it is given, byte for byte, and gives it back through both
// accessors; a text a filter changed is followed by RemovalMark, which
// Marked keeps and WithoutMark leaves out.
func TestFilteredKeepsItsText(t *testing.T) {
	t.Parallel()
	const raw = "a\u200bb\xff\n"
	for name, f := range map[string]Filtered{
		"Plain":                Plain(raw),
		"NewFiltered as it is": NewFiltered(raw, false),
	} {
		assert.Equal(t, raw, f.Marked(), "%s: Marked gives the text back", name)
		assert.Equal(t, raw, f.WithoutMark(), "%s: WithoutMark gives the text back", name)
	}
	changed := NewFiltered(raw, true)
	assert.Equal(t, raw+RemovalMark, changed.Marked(), "NewFiltered changed: the mark follows the text")
	assert.Equal(t, raw, changed.WithoutMark(), "NewFiltered changed: WithoutMark leaves the mark out")
	assert.Equal(t, raw+RemovalMark, Plain(raw+RemovalMark).Marked(), "Plain keeps a mark it is given")
}

// TestFilteredJoinsTextWhole asserts that Append, AppendString and
// JoinFiltered write every operand whole and in order, the separator
// included.
func TestFilteredJoinsTextWhole(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "abc", Plain("a").Append(Plain("b"), Plain("c")).Marked())
	assert.Equal(t, "a", Plain("a").Append().Marked())
	assert.Equal(t, "ab c", Plain("a").AppendString("b c").Marked())
	assert.Equal(t, "a, b, c", JoinFiltered([]Filtered{Plain("a"), Plain("b"), Plain("c")}, ", ").Marked())
	assert.Equal(t, "a", JoinFiltered([]Filtered{Plain("a")}, ", ").Marked())
	assert.True(t, JoinFiltered(nil, ", ").IsEmpty())
}

// TestFilteredIsEmptyOnlyForNoText asserts that the zero Filtered and an
// empty text are empty and that a text of one space is not.
func TestFilteredIsEmptyOnlyForNoText(t *testing.T) {
	t.Parallel()
	assert.True(t, Filtered{}.IsEmpty())
	assert.True(t, Plain("").IsEmpty())
	assert.False(t, Plain(" ").IsEmpty())
	assert.False(t, Plain("\u200b").IsEmpty())
}

// TestFilteredWidthCountsRunes asserts that Width counts each rune once
// whatever its byte length, and an invalid byte as one.
func TestFilteredWidthCountsRunes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0, Filtered{}.Width())
	assert.Equal(t, 3, Plain("abc").Width())
	assert.Equal(t, 3, Plain("a\u00e9\u4e16").Width(), "two- and three-byte runes count once")
	assert.Equal(t, 2, Plain("a\xff").Width(), "an invalid byte counts once")
}

// TestFilteredTrimsAsTheStringsFunctions asserts that TrimSuffix removes one
// suffix and that TrimRight removes the trailing runes of its cutset, and
// nothing else.
func TestFilteredTrimsAsTheStringsFunctions(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "a\n", Plain("a\n\n").TrimSuffix("\n").Marked(), "one suffix")
	assert.Equal(t, "ab", Plain("ab").TrimSuffix("\n").Marked(), "no suffix")
	assert.Equal(t, "a", Plain("a  ").TrimRight(" ").Marked())
	assert.Equal(t, "a", Plain("axyx").TrimRight("xy").Marked(), "every rune of the cutset")
	assert.Equal(t, "ax ", Plain("ax ").TrimRight("x").Marked(), "a rune outside the cutset stops the trim")
}

// TestFilteredRefusesFmtAndEncoders asserts that fmt prints a refusal for a
// Filtered under every verb, and that encoding/json and MarshalText return an
// error, never the text.
func TestFilteredRefusesFmtAndEncoders(t *testing.T) {
	t.Parallel()
	f := Plain("secret-ish text")
	var _ = Filtered.Format
	assert.Equal(t, "%!v(clierr.Filtered)", fmt.Sprint(f))
	assert.Equal(t, "%!s(clierr.Filtered)", fmt.Sprintf("%s", f))
	assert.Equal(t, "%!v(clierr.Filtered)", fmt.Sprintf("%.3v", f))
	assert.Equal(t, "%!q(clierr.Filtered)", fmt.Sprintf("%q", f))

	var _ fmt.Formatter = f
	_, err := json.Marshal(struct{ F Filtered }{f})
	require.ErrorContains(t, err, "clierr.Filtered cannot be encoded")
	require.ErrorIs(t, err, errFilteredEncoding)
	_, err = f.MarshalJSON()
	require.ErrorIs(t, err, errFilteredEncoding)
	_, err = json.Marshal(f)
	require.ErrorContains(t, err, "clierr.Filtered cannot be encoded")
	text, err := f.MarshalText()
	require.ErrorContains(t, err, "clierr.Filtered cannot be encoded")
	assert.Nil(t, text)
}

// TestSprintfWritesFilteredArgumentsWhole asserts that Sprintf writes a
// Filtered argument whole for %s and %v, pads it by Width for a width,
// ignores a precision, writes the refusal for any other verb, and formats
// every other argument as fmt does.
func TestSprintfWritesFilteredArgumentsWhole(t *testing.T) {
	t.Parallel()
	ab := Plain("ab")
	var _ fmt.Formatter = whole{ab}
	var _ = whole.Format
	for _, tc := range []struct {
		name string
		got  Filtered
		want string
	}{
		{"%s", Sprintf("[%s]", ab), "[ab]"},
		{"%v", Sprintf("[%v]", ab), "[ab]"},
		{"a width pads on the left", Sprintf("[%4s]", ab), "[  ab]"},
		{"the - flag pads on the right", Sprintf("[%-4s]", ab), "[ab  ]"},
		{"a width counts runes", Sprintf("[%-3s]", Plain("\u00e9")), "[\u00e9  ]"},
		{"a width below the text pads nothing", Sprintf("[%1s]", ab), "[ab]"},
		{"a precision is ignored", Sprintf("[%.1s]", ab), "[ab]"},
		{"a long value under a precision stays whole", Sprintf("%.40s", Plain("namespace-with-a-name-longer-than-forty-characters")), "namespace-with-a-name-longer-than-forty-characters"},
		{"%q is refused", Sprintf("%q", ab), "%!q(clierr.Filtered)"},
		{"%d is refused", Sprintf("%d", ab), "%!d(clierr.Filtered)"},
		{"other arguments as fmt formats them", Sprintf("%s %q %d %v", ab, "x", 3, true), `ab "x" 3 true`},
	} {
		assert.Equal(t, tc.want, tc.got.Marked(), tc.name)
	}
}

// TestBuilderWritesEveryPartInOrder asserts that a Builder keeps every
// write, of each method, in order, and that Printf pads by Width.
func TestBuilderWritesEveryPartInOrder(t *testing.T) {
	t.Parallel()
	var b Builder
	assert.True(t, b.Filtered().IsEmpty())
	b.WriteString("a")
	b.Write(Plain("b"))
	b.Printf("%s-%d", Plain("c"), 1)
	b.Printf("[%-4s]", Plain("d"))
	b.WriteString("e")
	assert.Equal(t, "abc-1[d   ]e", b.Filtered().Marked())
}

// TestTextOfWritesItsFirstPartWhole asserts that TextOf writes the Filtered
// text first and its parts as Text writes them: a Lead before a printed
// command is written, a Lead before a left-out command is dropped and the
// note of a left-out command follows.
func TestTextOfWritesItsFirstPartWhole(t *testing.T) {
	t.Parallel()
	first := Plain("the value a\u200bb was rejected")
	printed := NewCommand("get pods").Arg("web")
	assert.Equal(t, Text(first.Marked(), Lead("; run: "), printed), TextOf(first, Lead("; run: "), printed).Marked())
	assert.Equal(t, "the value a\u200bb was rejected; run: rancher get pods 'web'", plain(TextOf(first, Lead("; run: "), printed).Marked()))

	leftOut := NewCommand("get pods").Arg("w\u200beb")
	assert.Equal(t, "the value a\u200bb was rejected; "+LeftOutNote, TextOf(first, Lead("; run: "), leftOut).Marked())
}

// TestTextOfPanicsOnAFilteredPart asserts that a Filtered among the parts of
// TextOf panics instead of being written: only the first part may be one.
func TestTextOfPanicsOnAFilteredPart(t *testing.T) {
	t.Parallel()
	require.PanicsWithValue(t, "clierr.TextOf: pass a Filtered as first, not in parts", func() {
		TextOf(Plain("a"), Lead("; "), Plain("b"))
	})
}

// TestTextOfMovesTheNoteAsTextDoes asserts that a first part with the
// note of a left-out command comes back as Text gives it for the same
// string: NoteOnce moves the note to the end, as it does for every text
// Text writes, and no character of the first part is dropped.
func TestTextOfMovesTheNoteAsTextDoes(t *testing.T) {
	t.Parallel()
	first := "server said: x; " + LeftOutNote + " y"
	leftOut := NewCommand("get pods").Arg("w\u200beb")
	assert.Equal(t, Text(first, Lead("; run: "), leftOut), TextOf(Plain(first), Lead("; run: "), leftOut).Marked())
	assert.Equal(t, Text(first), TextOf(Plain(first)).Marked())
}

// TestFilteredKeepsLongTextWhole asserts that every method and function of
// filtered.go keeps all of a 64 KiB text: a cut at any length below that
// fails.
func TestFilteredKeepsLongTextWhole(t *testing.T) {
	t.Parallel()
	v := strings.Repeat("abcdefgh", 8192)
	f := Plain(v)
	var b Builder
	b.WriteString(v)
	b.Write(f)
	b.Printf("%s%-3s%.3v", f, f, f)
	for name, got := range map[string]string{
		"Plain":        f.Marked(),
		"NewFiltered":  NewFiltered(v, true).WithoutMark(),
		"WithoutMark":  f.WithoutMark(),
		"Append":       Plain("").Append(f).Marked(),
		"AppendString": Plain("").AppendString(v).Marked(),
		"JoinFiltered": JoinFiltered([]Filtered{f}, ", ").Marked(),
		"TrimSuffix":   f.TrimSuffix("x").Marked(),
		"TrimRight":    f.TrimRight("x").Marked(),
		"Sprintf":      Sprintf("%.3s", f).Marked(),
		"TextOf":       TextOf(f).Marked(),
	} {
		assert.Equal(t, len(v), len(got), name)
	}
	assert.Equal(t, len(v), f.Width())
	assert.Equal(t, 5*len(v), len(b.Filtered().Marked()), "Builder")
}

// TestFilteredKeepsTheMark asserts that every way to build a Filtered from
// other Filtered values keeps the RemovalMark of each input that had one,
// and adds none to an input that had none: the renderer of the text finds
// the mark wherever it is.
func TestFilteredKeepsTheMark(t *testing.T) {
	t.Parallel()
	marked, unmarked := NewFiltered("a", true), Plain("b")
	var b Builder
	b.WriteString("x")
	b.Write(marked)
	b.Printf("%s", marked)
	for _, tc := range []struct {
		name  string
		got   Filtered
		marks int
	}{
		{"Append, the marked operand first", marked.Append(unmarked), 1},
		{"Append, the marked operand last", unmarked.Append(unmarked, marked), 1},
		{"Append of two marked operands", marked.Append(marked), 2},
		{"Append with nothing marked", unmarked.Append(unmarked), 0},
		{"AppendString", marked.AppendString(" c"), 1},
		{"JoinFiltered", JoinFiltered([]Filtered{unmarked, marked}, ", "), 1},
		{"JoinFiltered with nothing marked", JoinFiltered([]Filtered{unmarked, unmarked}, ", "), 0},
		{"Sprintf of a marked argument", Sprintf("[%s]", marked), 1},
		{"Sprintf pads a marked argument", Sprintf("[%-3v]", marked), 1},
		{"Builder.WriteString, Builder.Write and Builder.Printf through Builder.Filtered", b.Filtered(), 2},
		{"TextOf with a marked first part", TextOf(marked, Lead("; run: "), NewCommand("get pods")), 1},
		{"TextOf with a left-out command", TextOf(marked, Lead("; run: "), NewCommand("get pods").Arg("w\u200beb")), 1},
	} {
		assert.Equal(t, tc.marks, strings.Count(tc.got.Marked(), RemovalMark), tc.name)
		assert.NotContains(t, tc.got.WithoutMark(), RemovalMark, tc.name)
	}
	assert.Equal(t, "[a  ]", Sprintf("[%-3v]", marked).WithoutMark(), "the width pads by the characters a terminal shows")
	assert.Equal(t, "%!v(clierr.Filtered)", fmt.Sprint(marked), "Format writes the refusal and no mark")
	_, err := marked.MarshalJSON()
	require.ErrorIs(t, err, errFilteredEncoding, "MarshalJSON refuses a marked value too")
	_, err = marked.MarshalText()
	require.ErrorIs(t, err, errFilteredEncoding, "MarshalText refuses a marked value too")
}

// TestFilteredTrimsAndMeasuresWithoutTheMark asserts that TrimSuffix and
// TrimRight trim the text a terminal shows and end the result with one
// RemovalMark when the input had one, that Width counts no character of the
// mark, and that a text that is only the mark is not empty.
func TestFilteredTrimsAndMeasuresWithoutTheMark(t *testing.T) {
	t.Parallel()
	line := NewFiltered("a", true).AppendString("  \n")
	assert.Equal(t, "a  "+RemovalMark, line.TrimSuffix("\n").Marked(), "TrimSuffix finds the suffix the mark is before")
	assert.Equal(t, "a"+RemovalMark, line.TrimRight(" \n").Marked(), "TrimRight trims the runes the mark is before")
	twice := NewFiltered("a", true).AppendString(" ").Append(NewFiltered("b", true)).AppendString(" ")
	assert.Equal(t, "a b"+RemovalMark, twice.TrimRight(" ").Marked(), "one mark at the end for any number of marks")
	assert.Equal(t, "a", Plain("a ").TrimRight(" ").Marked(), "no mark is added to a text without one")
	assert.Equal(t, "a", Plain("a\n").TrimSuffix("\n").Marked(), "no mark is added to a text without one")

	assert.Equal(t, 1, NewFiltered("a", true).Width(), "Width counts no character of the mark")
	assert.Equal(t, 4, twice.Width(), "a, a space, b and a space")
	assert.Equal(t, 0, NewFiltered("", true).Width())
	assert.False(t, NewFiltered("", true).IsEmpty(), "a value the filter emptied is not empty")
	assert.True(t, NewFiltered("", false).IsEmpty())
}

// TestFilteredIsNotComparable asserts that the compiler refuses == on a
// Filtered and a Filtered as a map key: the mark of a changed value would make
// two values that print the same compare as different.
func TestFilteredIsNotComparable(t *testing.T) {
	t.Parallel()
	assert.False(t, reflect.TypeFor[Filtered]().Comparable())
}
