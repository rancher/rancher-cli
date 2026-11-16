package clierr

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// textFunction names one of the three value text functions.
type textFunction struct {
	name string
	fn   func(string) string
}

func textFunctions() []textFunction {
	return []textFunction{
		{"LineText", LineText},
		{"ValueText", ValueText},
		{"WholeValueText", WholeValueText},
	}
}

// allStrings returns every string of 0 to maxLen items.
func allStrings(items []string, maxLen int) []string {
	all := []string{""}
	level := []string{""}
	for range maxLen {
		var next []string
		for _, prefix := range level {
			for _, item := range items {
				next = append(next, prefix+item)
			}
		}
		all = append(all, next...)
		level = next
	}
	return all
}

// cutPieces cuts s at each Rancher (the remedy marker followed by "rancher"),
// "; ", " (" and single quote, taking the earliest each time, and returns the
// pieces between them.
func cutPieces(s string) []string {
	separators := []string{Rancher, "; ", " (", "'"}
	var pieces []string
	for {
		at, width := -1, 0
		for _, separator := range separators {
			if i := strings.Index(s, separator); i >= 0 && (at < 0 || i < at) {
				at, width = i, len(separator)
			}
		}
		if at < 0 {
			return append(pieces, s)
		}
		pieces = append(pieces, s[:at])
		s = s[at+width:]
	}
}

// hasQuotedPiece reports whether a piece of s, cut as cutPieces cuts it, is
// exactly the double-quoted form strconv.Quote writes for a text NotPrintable
// reports true for.
func hasQuotedPiece(s string) bool {
	return slices.ContainsFunc(cutPieces(s), quotedSegment)
}

// escapedFormOfNotPrintableText reports whether v is exactly the double-quoted
// form strconv.Quote writes for a text that holds a hidden rune or an invalid
// byte.
func escapedFormOfNotPrintableText(v string) bool {
	u, err := strconv.Unquote(v)
	return err == nil && strconv.Quote(u) == v && (!utf8.ValidString(u) || strings.IndexFunc(u, IsUnsafeToDisplay) >= 0)
}

// exemptFromDistinctOutputs reports whether s is an input that may print like
// another input. That is when a piece of s is exactly the double-quoted form
// strconv.Quote writes for a text NotPrintable reports true for. The pieces
// are those cutPieces returns, the whole of s, and the pieces of s cut at
// single quotes only (hasQuotedSegment).
func exemptFromDistinctOutputs(s string) bool {
	return hasQuotedPiece(s) || hasQuotedSegment(s)
}

// assertDistinctOutputs asserts that two different inputs give different
// outputs unless one of them is exempt (see exemptFromDistinctOutputs).
func assertDistinctOutputs(t *testing.T, fn func(string) string, inputs []string) {
	t.Helper()
	first := map[string]string{}
	for _, in := range inputs {
		out := fn(in)
		prev, seen := first[out]
		if !seen {
			first[out] = in
			continue
		}
		if prev == in {
			continue
		}
		assert.True(t, exemptFromDistinctOutputs(prev) || exemptFromDistinctOutputs(in), "%q and %q both give %q", prev, in, out)
	}
}

// assertNoHiddenOutput asserts the output holds no rune IsUnsafeToDisplay
// matches and no invalid byte once every Rancher is removed.
func assertNoHiddenOutput(t *testing.T, in, out string) {
	t.Helper()
	kept := strings.ReplaceAll(out, Rancher, "")
	assert.True(t, utf8.ValidString(kept), "%q gives an invalid byte: %q", in, out)
	assert.Less(t, strings.IndexFunc(kept, IsUnsafeToDisplay), 0, "%q gives a hidden rune: %q", in, out)
}

// assertNoSingleQuoteInQuotes asserts that, for an input with no double
// quote, no double-quoted piece the function wrote holds a single quote.
func assertNoSingleQuoteInQuotes(t *testing.T, in, out string) {
	t.Helper()
	if strings.Contains(in, `"`) {
		return
	}
	pieces := strings.Split(out, `"`)
	for i := 1; i < len(pieces)-1; i += 2 {
		assert.NotContains(t, pieces[i], "'", "%q gives a single quote inside double quotes: %q", in, out)
	}
}

// TestValueTextFunctionsOverEveryShortString asserts, for each of LineText,
// ValueText and WholeValueText, over every string of 0 to 4 items drawn from a
// list of characters and character sequences that includes those the
// functions treat specially: the output, with each Rancher removed, holds no
// hidden rune and no invalid byte; for an input with no double quote, no
// double-quoted piece the function writes holds a single quote; an input that
// holds nothing to escape besides Rancher comes back unchanged, except that
// WholeValueText writes in double quotes again an input that is the escaped
// form of a text NotPrintable reports true for and holds no single quote; and
// two different inputs give different outputs unless one of them is exempt
// (see exemptFromDistinctOutputs).
func TestValueTextFunctionsOverEveryShortString(t *testing.T) {
	t.Parallel()
	inputs := allStrings([]string{"a", "'", ";", " ", "(", `"`, `\`, "\a", "\u200b", "\xff", Rancher, "; ", " (", `\u200b`}, 4)
	require.Len(t, inputs, 41371)
	for _, tf := range textFunctions() {
		t.Run(tf.name, func(t *testing.T) {
			t.Parallel()
			for _, in := range inputs {
				out := tf.fn(in)
				assertNoHiddenOutput(t, in, out)
				assertNoSingleQuoteInQuotes(t, in, out)
				if !NotPrintable(strings.ReplaceAll(in, Rancher, "")) {
					if tf.name == "WholeValueText" && escapedFormOfNotPrintableText(in) && !strings.Contains(in, "'") {
						assert.Equal(t, strconv.Quote(in), out, "input %q", in)
					} else {
						assert.Equal(t, in, out)
					}
				}
			}
			assertDistinctOutputs(t, tf.fn, inputs)
		})
	}
}

// TestValueTextFunctionsMatchIDTextForEveryIDTextCase asserts that for each
// case of idTextCases, none of which holds Rancher or is the escaped form of a
// text NotPrintable reports true for, each function gives what IDText gives,
// and that each invalid byte alone is written as a double-quoted Go string.
func TestValueTextFunctionsMatchIDTextForEveryIDTextCase(t *testing.T) {
	t.Parallel()
	for _, tc := range idTextCases() {
		require.NotContains(t, tc.in, Rancher, tc.name)
		require.False(t, escapedFormOfNotPrintableText(tc.in), tc.name)
		for _, tf := range textFunctions() {
			t.Run(tf.name+" "+tc.name, func(t *testing.T) {
				t.Parallel()
				assert.Equal(t, IDText(tc.in), tf.fn(tc.in))
			})
		}
	}
	for b := 0x80; b <= 0xff; b++ {
		alone := string([]byte{byte(b)})
		for _, tf := range textFunctions() {
			t.Run(tf.name+" byte "+strconv.Itoa(b), func(t *testing.T) {
				t.Parallel()
				assert.Equal(t, strconv.Quote(alone), tf.fn(alone))
			})
		}
	}
}

// TestWholeValueTextOverEveryShortStringOfQuotesAndEscapes asserts, over every
// string of 0 to 5 items drawn from the double quote, the single quote, the
// backslash, a letter, the bell character and the six characters `\u200b`,
// that WholeValueText writes no hidden rune and no invalid byte; writes in
// double quotes again each escaped form of a text NotPrintable reports true
// for that holds no single quote; leaves an escaped form that holds a single
// quote as it is; writes no single quote in an output that is one
// double-quoted string and differs from its input; and gives different inputs
// different outputs under the same exemption as the other functions.
func TestWholeValueTextOverEveryShortStringOfQuotesAndEscapes(t *testing.T) {
	t.Parallel()
	inputs := allStrings([]string{`"`, "'", `\`, "a", "\a", `\u200b`}, 5)
	require.Len(t, inputs, 9331)
	for _, in := range inputs {
		out := WholeValueText(in)
		assert.True(t, utf8.ValidString(out), "%q gives an invalid byte: %q", in, out)
		assert.Less(t, strings.IndexFunc(out, IsUnsafeToDisplay), 0, "%q gives a hidden rune: %q", in, out)
		switch {
		case escapedFormOfNotPrintableText(in) && !strings.Contains(in, "'"):
			assert.Equal(t, strconv.Quote(in), out, "input %q", in)
		case escapedFormOfNotPrintableText(in):
			assert.Equal(t, in, out, "input %q", in)
		}
		if out != in && strings.HasPrefix(out, `"`) {
			if _, err := strconv.Unquote(out); err == nil {
				assert.NotContains(t, out, "'", "input %q", in)
			}
		}
	}
	assertDistinctOutputs(t, WholeValueText, inputs)
}

// TestLineTextRows asserts the exact output of LineText: only a clause that
// holds a hidden rune or an invalid byte is quoted, and each Rancher and each
// clause boundary stays as written and outside the quotes.
func TestLineTextRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"printable text", "denied", "denied"},
		{"a hidden rune before a command", "a\u200bb; " + Rancher + " get x", `"a\u200bb"; ` + Rancher + ` get x`},
		{"a hidden rune before a parenthesis", "user u\u200b (" + Rancher + " get users)", `"user u\u200b" (` + Rancher + ` get users)`},
		{"a hidden rune in a quoted argument", "run " + Rancher + " get users 'u\u200b'", "run " + Rancher + ` get users '"u\u200b"'`},
		{"a command clause with a hidden rune", "see; " + Rancher + " get x\u200b", "see; " + Rancher + `" get x\u200b"`},
		{"two commands", "a\u200b" + Rancher + " get x\u200b; " + Rancher + " get y", `"a\u200b"` + Rancher + `" get x\u200b"; ` + Rancher + ` get y`},
		{"an invalid byte", "x\xff; y", `"x\xff"; y`},
		{"a newline", "a\nb; c", `"a\nb"; c`},
		{"a carriage return and a newline", "a\r\nb", `"a\r\nb"`},
		{"a line separator", "a\u2028b", `"a\u2028b"`},
		{"a paragraph separator", "a\u2029b", `"a\u2029b"`},
		{"a double quote in the clause", "a\"b\u200b", `"a\"b\u200b"`},
		{"a backslash in the clause", "a\\b\u200b", `"a\\b\u200b"`},
		{"a single quote and a hidden rune", "o'b\u200b; c", `o'"b\u200b"; c`},
		{"a single quote written as '\\''", "it'\\''s\u200b", `it'\''"s\u200b"`},
		{"a bare boundary", "; (", "; ("},
		{"a hidden rune in each clause", "\u200b; \u200b (\u200b", `"\u200b"; "\u200b" ("\u200b"`},
		{"the earliest boundary wins", "a\u200b; (b\u200b", `"a\u200b"; "(b\u200b"`},
		{"another process's marker", "x\u2063\u2060rancher get y", `"x\u2063\u2060rancher get y"`},
		{"a marker with no command after it", "a" + RemedyMarker + "b", strconv.Quote("a" + RemedyMarker + "b")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, LineText(tc.in))
		})
	}
}

// TestValueTextRows asserts ValueText writes a value as a whole unless it
// holds this process's remedy marker, and in that case writes it clause by
// clause as LineText does.
func TestValueTextRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"printable", "u-abc", "u-abc"},
		{"a hidden rune and a parenthesis, no marker", "u\u200b (x)", `"u\u200b (x)"`},
		{"a command after a clause", "see; " + Rancher + " get x\u200b", "see; " + Rancher + `" get x\u200b"`},
		{"a marker with no command after it", "a" + RemedyMarker + "b", strconv.Quote("a" + RemedyMarker + "b")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, ValueText(tc.in))
		})
	}
	assert.Equal(t, `"u\u200b" (x)`, LineText("u\u200b (x)"))
	assert.NotEqual(t, LineText("u\u200b (x)"), ValueText("u\u200b (x)"))
}

// TestWholeValueTextRows asserts the exact output of WholeValueText for a
// value whose start and end are known.
func TestWholeValueTextRows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"the escaped form of a text with a hidden character", `"a\u200bb"`, `"\"a\\u200bb\""`},
		{"a text with a hidden character", "a\u200bb", `"a\u200bb"`},
		{"the escaped form of an invalid byte", `"a\xffb"`, `"\"a\\xffb\""`},
		{"a double-quoted variable", `"$PASSWORD"`, `"$PASSWORD"`},
		{"a double-quoted word", `"ops"`, `"ops"`},
		{"a double-quoted word in single quotes", `'"a"'`, `'"a"'`},
		{"a double-quoted word in single quotes again", `'"ops"'`, `'"ops"'`},
		{"two double quotes", `""`, `""`},
		{"an escape in capital letters", `"\u200B"`, `"\u200B"`},
		{"a rune literal", `'\u200b'`, `'\u200b'`},
		{"an escaped form with a single quote", `"o'b\u200b"`, `"o'b\u200b"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, WholeValueText(tc.in))
		})
	}
	assert.NotEqual(t, WholeValueText(`"a\u200bb"`), WholeValueText("a\u200bb"))
	// The text `"\"a\\u200bb\""` unquotes to printable text and prints as
	// written. That is also what WholeValueText gives for `"a\u200bb"`: these
	// two different inputs print the same, which the exemption in
	// assertDistinctOutputs allows.
	assert.Equal(t, `"\"a\\u200bb\""`, WholeValueText(`"\"a\\u200bb\""`))
	assert.Equal(t, WholeValueText(`"\"a\\u200bb\""`), WholeValueText(`"a\u200bb"`))
	command := "x " + Rancher + " get y\u200b"
	assert.Equal(t, ValueText(command), WholeValueText(command))
}
