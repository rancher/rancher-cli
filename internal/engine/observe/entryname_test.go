package observe

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCheckComponentAcceptsPlainNames asserts that every name Kubernetes and
// Rancher produce for a cluster id, namespace, pod, container or target, and
// the segments the bundle builds from them, is accepted. A reserved Windows
// name inside a longer segment is a plain name.
func TestCheckComponentAcceptsPlainNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"local",
		"c-m-abcd1234",
		"events-cattle-system.json",
		"events-con.json",
		"pod-1-main-previous.log",
		strings.Repeat("a", 255),
		"a_b",
		"events-...json",
		"README.txt",
		"fleet-agent",
	} {
		assert.NoError(t, checkComponent(name), "%q", name)
	}
}

// TestCheckComponentRefusesUnsafeNames asserts that each kind of value that
// changes where an extractor writes, or that a platform reads as another
// name, is refused with a reason that names what is wrong.
func TestCheckComponentRefusesUnsafeNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{"empty", "", "it is empty"},
		{"dot", ".", `it is "." or ".."`},
		{"dot dot", "..", `it is "." or ".."`},
		{"slash inside", "a/b", "it has a '/'"},
		{"leading slash", "/a", "it has a '/'"},
		{"backslash", `a\b`, `it has a '\\'`},
		{"colon", "a:b", "it has a ':'"},
		{"drive letter", `C:\x`, "it has a ':'"},
		{"NUL", "a\x00b", `it has a '\x00'`},
		{"newline", "a\nb", `it has a '\n'`},
		{"escape sequence", "a\x1b[31m", `it has a '\x1b'`},
		{"right-to-left override", "a\u202eb", "it has a non-ASCII character"},
		{"accented letter", "caf\u00e9", "it has a non-ASCII character"},
		{"too long", strings.Repeat("a", 256), "is longer than 255 bytes"},
		{"trailing dot", "a.", "it ends with a '.'"},
		{"trailing space", "a ", "it has a ' '"},
		{"reserved CON", "con", "starts with a name reserved on Windows"},
		{"reserved NUL", "NUL", "starts with a name reserved on Windows"},
		{"reserved mixed case COM1", "Com1", "starts with a name reserved on Windows"},
		{"reserved with extension", "con.txt", "starts with a name reserved on Windows"},
		{"reserved LPT9", "lpt9", "starts with a name reserved on Windows"},
		{"reserved before the first dot", "aux.example-main.log", "starts with a name reserved on Windows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.ErrorContains(t, checkComponent(tc.value), tc.want)
		})
	}
}

// TestCheckComponentErrorQuotesTheValue asserts that a control character in
// a refused value prints as an escape, never as the raw byte.
func TestCheckComponentErrorQuotesTheValue(t *testing.T) {
	t.Parallel()
	err := checkComponent("a\x1b[31m")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "\x1b")
	require.ErrorContains(t, err, `"a\x1b[31m"`)
}

func TestEntryPathJoinsComponents(t *testing.T) {
	t.Parallel()
	name, err := entryPath("clusters", "c-1", "cluster.json")
	require.NoError(t, err)
	assert.Equal(t, entryName{entryText: "clusters/c-1/cluster.json"}, name)
}

func TestEntryPathRefusesUnsafeComponent(t *testing.T) {
	t.Parallel()
	_, err := entryPath("clusters", "../x", "cluster.json")
	require.ErrorContains(t, err, "is not a plain file name")

	_, err = entryPath("clusters", "a/b", "cluster.json")
	require.ErrorContains(t, err, "it has a '/'")
}

func TestEntryPathRefusesNoComponents(t *testing.T) {
	t.Parallel()
	_, err := entryPath()
	require.Error(t, err)
}

// TestEntryPathChecksTheWholeSegment asserts that the rule applies to each
// segment as built: a namespace that is "..", or a reserved Windows name,
// inside a longer file name gives a plain name.
func TestEntryPathChecksTheWholeSegment(t *testing.T) {
	t.Parallel()
	for ns, want := range map[string]string{
		"..":  "clusters/c-1/events-...json",
		"con": "clusters/c-1/events-con.json",
	} {
		name, err := entryPath("clusters", "c-1", "events-"+ns+".json")
		require.NoError(t, err, ns)
		assert.Equal(t, want, name.entryText)
	}
}

func TestStaticEntryPathPanicsOnRefusedName(t *testing.T) {
	t.Parallel()
	assert.Panics(t, func() { staticEntryPath("a/../b") })
	assert.Panics(t, func() { staticEntryPath("") })
	assert.Panics(t, func() { staticEntryPath("/manifest.json") })
}

// TestStaticEntryPathAcceptsTheFixedNames asserts every fixed entry name of
// the bundle is accepted; a typo in one fails here and never in a user's
// bundle after a collector succeeded.
func TestStaticEntryPathAcceptsTheFixedNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"rancher/settings.json",
		"rancher/features.json",
		"rancher/schema.json",
		"rancher/version.json",
		"README.txt",
		"manifest.json",
	} {
		assert.NotPanics(t, func() {
			assert.Equal(t, name, staticEntryPath(name).entryText)
		}, name)
	}
}
