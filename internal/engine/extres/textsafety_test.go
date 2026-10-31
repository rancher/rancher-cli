package extres

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// hostileTail holds an escape sequence introducer, the one-rune control
// sequence introducer U+009B, a tab, U+202E RIGHT-TO-LEFT OVERRIDE, U+0085
// NEXT LINE, U+2028 LINE SEPARATOR, a carriage return and a line feed, then
// a line that reads as the CLI's own.
const hostileTail = "\x1b[2J\u009b2J\t\u202e\u0085\u2028\r\nforged line"

func hostileValue(p string) string { return "<" + p + ">" + hostileTail }

// assertLineRule asserts that out holds the angle-bracketed path of every
// entry of present, holds nothing the writer filter removes, and has no line
// that starts with the forged line after its leading spaces.
func assertLineRule(t *testing.T, out string, present []string) {
	t.Helper()
	for _, p := range present {
		assert.Contains(t, out, "<"+p+">", "the text prints %s", p)
	}
	assert.Equal(t, output.PrintableLines(out), out, "the text holds no character the writer filter removes")
	for line := range strings.Lines(out) {
		assert.False(t, strings.HasPrefix(strings.TrimLeft(line, " "), "forged line"), "a line starts with the forged line: %q", line)
	}
}

func TestCreateTokenTextKeepsServerValuesOnTheirLines(t *testing.T) {
	t.Parallel()
	name, bearer, expiresAt := hostileValue("name"), hostileValue("bearerToken"), hostileValue("expiresAt")
	client := &fakeClient{
		name:   name,
		status: map[string]any{"bearerToken": bearer, "expiresAt": expiresAt},
	}
	_, exec, err := planToken(t, NewCreateToken(stubFactory{c: client}), in{s: map[string]string{}})
	require.NoError(t, err)
	require.NotNil(t, exec)

	res, err := exec(t.Context())
	require.NoError(t, err)

	assertLineRule(t, res.Text, []string{"name", "expiresAt"})
	assert.Contains(t, res.Text, strconv.Quote(bearer), "the bearer token is given in its quoted form")
	assert.Contains(t, res.Text, runtime.QuotedValueNote)

	data, ok := res.Data.(map[string]string)
	require.True(t, ok)
	assert.Equal(t, map[string]string{"name": name, "bearerToken": bearer, "expiresAt": expiresAt}, data, "json and yaml carry the raw values")
}
