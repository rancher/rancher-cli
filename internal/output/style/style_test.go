package style

import (
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
