package runtime

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output/style"
)

// unprintableDir is a config directory whose path holds a newline, which a
// terminal line cannot show.
const unprintableDir = "/home/u/a\nb"

func styled(s string) string { return "\x1b[1m" + s + "\x1b[0m" }

// TestExpandLinesForAnUnprintableDirectoryKeepsEveryOtherLine asserts that,
// for a directory no command can name, a line that holds a command keeps its
// words up to the clause with the first command and then says why no command
// is offered, a command in one line never changes another line, a styled
// line keeps its reset code, and a text with no command is returned as it is.
func TestExpandLinesForAnUnprintableDirectoryKeepsEveryOtherLine(t *testing.T) {
	t.Parallel()
	streams := IO{RemedyDir: unprintableDir}
	cmd := clierr.Rancher + " config status"
	for _, tc := range []struct{ name, in, want string }{
		{
			"a command in the first clause of the second line",
			"first line\nrun " + cmd + "\nthird line\n",
			"first line\n" + UnprintableConfigDirNote + "\nthird line\n",
		},
		{
			"a command in the second clause",
			"before; see the list; then run " + cmd + " now\nnext line\n",
			"before; see the list; " + UnprintableConfigDirNote + "\nnext line\n",
		},
		{
			"a command in a clause that follows a parenthesis",
			"first\nthe list (run " + cmd + ")\nlast\n",
			"first\nthe list; " + UnprintableConfigDirNote + "\nlast\n",
		},
		{
			"a styled line with the command in its first clause",
			"first\n" + styled("run "+cmd) + "\nlast\n",
			"first\n" + UnprintableConfigDirNote + "\x1b[0m\nlast\n",
		},
		{
			"a styled line with a clause before the command",
			"first\n" + styled("see the list; then run "+cmd) + "\nlast\n",
			"first\n\x1b[1msee the list; " + UnprintableConfigDirNote + "\x1b[0m\nlast\n",
		},
		{
			"a styled span that closes before the end of the line",
			"first\n\x1b[33mwarning; run " + cmd + "\x1b[0m (see above)\nnext line\n",
			"first\n\x1b[33mwarning; " + UnprintableConfigDirNote + "\x1b[0m\nnext line\n",
		},
		{
			"a styled line that ends with a carriage return and a newline",
			"first\r\n" + styled("a; run "+cmd) + "\r\nlast\r\n",
			"first\r\n\x1b[1ma; " + UnprintableConfigDirNote + "\x1b[0m\r\nlast\r\n",
		},
		{
			"a styled span closed before the clause with the command",
			"\x1b[1mbold\x1b[0m; run " + cmd + "\n",
			"\x1b[1mbold\x1b[0m; " + UnprintableConfigDirNote + "\n",
		},
		{
			"a command in the last line with no newline",
			"first\nrun " + cmd,
			"first\n" + UnprintableConfigDirNote,
		},
		{
			"a command on every line",
			"run " + cmd + "\nsee " + cmd + "\n",
			UnprintableConfigDirNote + "\n" + UnprintableConfigDirNote + "\n",
		},
		{"a text with no command", "one\ntwo; three (four)\n", "one\ntwo; three (four)\n"},
		{"an empty text", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := streams.ExpandLines(tc.in)
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "rancher ", "no command is offered")
			assert.NotContains(t, got, "--config")
			assert.NotContains(t, got, "/home/u/a")
			assert.False(t, strings.ContainsAny(got, "\u2060\u2061\u2062\u2063\u2064"), "no character of the marker is left: %q", got)
		})
	}
}

// TestExpandLinesForAPrintableDirectoryIsExpandText asserts that, for a
// directory a command can name, ExpandLines writes the same text as
// ExpandText: the directory after the word rancher in every command, and the
// same text when none holds one.
func TestExpandLinesForAPrintableDirectoryIsExpandText(t *testing.T) {
	t.Parallel()
	cmd := clierr.Rancher + " config status"
	for _, dir := range []string{"/d", ""} {
		streams := IO{RemedyDir: dir}
		for _, in := range []string{
			"first\nrun " + cmd + "\nsee " + cmd + "; then stop\n",
			styled("run "+cmd) + "\n",
			"no command here\n",
			"\u2063rancher delete x\n" + cmd,
			"",
		} {
			assert.Equal(t, streams.ExpandText(in), streams.ExpandLines(in), "directory %q, text %q", dir, in)
		}
	}
	assert.Equal(t, "run rancher --config '/d' config status\n", IO{RemedyDir: "/d"}.ExpandLines("run "+clierr.Rancher+" config status\n"))
	assert.Equal(t, "run rancher config status\n", IO{}.ExpandLines("run "+clierr.Rancher+" config status\n"))
}

// TestResetCodeIsWhatStyleWrites asserts the reset code ExpandLines keeps at
// the end of a styled line is the one every wrap method of style.Style writes
// after its text: each exported method of the form func(string) string, called
// on a word with styling enabled, ends with it.
func TestResetCodeIsWhatStyleWrites(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[style.Style]()
	var found []string
	for m := range typ.Methods() {
		ft := m.Type
		if ft.NumIn() != 2 || ft.In(1).Kind() != reflect.String || ft.NumOut() != 1 || ft.Out(0).Kind() != reflect.String {
			continue
		}
		found = append(found, m.Name)
		got := m.Func.Call([]reflect.Value{reflect.ValueOf(style.Style{Enabled: true}), reflect.ValueOf("word")})[0].String()
		assert.True(t, strings.HasSuffix(got, resetCode), "%s ends with %q, not the reset code %q", m.Name, got[max(0, len(got)-8):], resetCode)
	}
	for _, name := range []string{"Bold", "Red", "Yellow", "Cyan", "Green"} {
		require.Contains(t, found, name, "the wrap methods found by reflection")
	}
}

// BenchmarkExpandLinesWithNoRemedy measures ExpandLines on a text of 1 MiB
// that holds no command: the cost every printed text pays.
func BenchmarkExpandLinesWithNoRemedy(b *testing.B) {
	text := strings.Repeat("a line of ordinary text with no command in it\n", (1<<20)/46+1)
	streams := IO{RemedyDir: "/d"}
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		_ = streams.ExpandLines(text)
	}
}
