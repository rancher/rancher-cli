package style

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestStyleWrapsWhenEnabled(t *testing.T) {
	t.Parallel()
	s := Style{Enabled: true}
	assert.Equal(t, "\x1b[1mheader\x1b[0m", s.Bold("header"))
	assert.Equal(t, "\x1b[31merror\x1b[0m", s.Red("error"))
	assert.Equal(t, "\x1b[33mwarning\x1b[0m", s.Yellow("warning"))
	assert.Equal(t, "\x1b[36mhint\x1b[0m", s.Cyan("hint"))
	assert.Equal(t, "\x1b[32m[Prime]\x1b[0m", s.Green("[Prime]"))
}

func TestStyleNoopWhenDisabled(t *testing.T) {
	t.Parallel()
	s := Style{Enabled: false}
	assert.Equal(t, "header", s.Bold("header"))
	assert.Equal(t, "error", s.Red("error"))
	assert.Equal(t, "warning", s.Yellow("warning"))
	assert.Equal(t, "hint", s.Cyan("hint"))
	assert.Equal(t, "[Prime]", s.Green("[Prime]"))
}

func TestStyleNoopOnEmptyText(t *testing.T) {
	t.Parallel()
	s := Style{Enabled: true}
	assert.Empty(t, s.Bold(""))
	assert.Empty(t, s.Red(""))
	assert.Empty(t, s.Yellow(""))
	assert.Empty(t, s.Cyan(""))
	assert.Empty(t, s.Green(""))
}

func TestStripRoundTrip(t *testing.T) {
	t.Parallel()
	s := Style{Enabled: true}
	tests := []struct {
		name  string
		plain string
	}{
		{"bold", "header line"},
		{"red", "error (CODE): message"},
		{"yellow", "# warning: something happened"},
		{"cyan", "hint: try again"},
		{"empty", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, styled := range []string{s.Bold(tc.plain), s.Red(tc.plain), s.Yellow(tc.plain), s.Cyan(tc.plain), s.Green(tc.plain)} {
				assert.Equal(t, tc.plain, Strip(styled))
			}
		})
	}
}

// TestStripMultipleCodesInOneString asserts a line built from several wrap
// calls strips back to the exact concatenation of their plain inputs.
func TestStripMultipleCodesInOneString(t *testing.T) {
	t.Parallel()
	s := Style{Enabled: true}
	line := s.Red("error (CODE):") + " " + "message" + "\n" + "  " + s.Cyan("hint:") + " " + "try again"
	assert.Equal(t, "error (CODE): message\n  hint: try again", Strip(line))
}

func TestStripNoEscapesUnchanged(t *testing.T) {
	t.Parallel()
	plain := "just plain text, no codes here"
	assert.Equal(t, plain, Strip(plain))
}

// TestStripUnterminatedEscape asserts a malformed sequence with no
// terminating 'm' is left in place rather than silently eaten - Strip only
// removes sequences it can fully recognize.
func TestStripUnterminatedEscape(t *testing.T) {
	t.Parallel()
	malformed := "prefix \x1b[999 rest of line, no reset code"
	assert.Equal(t, malformed, Strip(malformed))
}

// TestUnstyledRemovesTheCodesOfEveryWrap asserts that, for each exported
// method of Style of the form func(string) string, found by reflection,
// Unstyled of the method's result for a word is the word. A wrap added later
// whose code Unstyled does not remove fails here, naming the method.
func TestUnstyledRemovesTheCodesOfEveryWrap(t *testing.T) {
	t.Parallel()
	s := Style{Enabled: true}
	typ := reflect.TypeOf(s)
	found := map[string]bool{}
	for m := range typ.Methods() {
		if m.Type.NumIn() != 2 || m.Type.NumOut() != 1 || m.Type.In(1).Kind() != reflect.String || m.Type.Out(0).Kind() != reflect.String {
			continue
		}
		found[m.Name] = true
		got := reflect.ValueOf(s).MethodByName(m.Name).Call([]reflect.Value{reflect.ValueOf("word")})[0].String()
		assert.NotEqual(t, "word", got, "%s writes a code when enabled", m.Name)
		assert.Equal(t, "word", Unstyled(got), "Unstyled removes the codes %s writes", m.Name)
	}
	for _, name := range []string{"Bold", "Red", "Yellow", "Cyan", "Green"} {
		assert.True(t, found[name], "%s is found by reflection", name)
	}
}

// TestUnstyledKeepsEverySequenceThatIsNotAWrapCode asserts that the conceal
// and clear-screen sequences and a code of another colour stay, so a text
// that adds one is not equal to its plain form, where Strip would remove
// the conceal and the colour.
func TestUnstyledKeepsEverySequenceThatIsNotAWrapCode(t *testing.T) {
	t.Parallel()
	for _, seq := range []string{"\x1b[8m", "\x1b[2J", "\x1b[35m", "\x1b[m", "\x1b]0;title\a"} {
		in := Style{Enabled: true}.Bold("a" + seq + "b")
		assert.Equal(t, "a"+seq+"b", Unstyled(in))
	}
	assert.Equal(t, "plain", Unstyled("plain"))
	assert.Equal(t, "", Unstyled(""))
	assert.Equal(t, "ab", Strip("a\x1b[8mb"), "Strip removes a conceal code, Unstyled does not")
}
