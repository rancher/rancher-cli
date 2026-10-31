package projects

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/output"
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

func TestRemoveNamespaceTextKeepsTheProjectOnItsLine(t *testing.T) {
	t.Parallel()
	project := hostileValue("project")
	ns := namespaceObj("team-a", project)
	client := &membershipClient{namespaces: map[string]*unstructured.Unstructured{"team-a": ns}}
	_, exec, err := planMembership(t, t.Context(), NewRemoveNamespace(stubFactory{c: client}), in{s: map[string]string{"cluster": "c-x"}, a: []string{"team-a"}})
	require.NoError(t, err)
	require.NotNil(t, exec)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assertLineRule(t, res.Text, []string{"project"})
	assert.Contains(t, res.Text, "removed from project <project>")
}
