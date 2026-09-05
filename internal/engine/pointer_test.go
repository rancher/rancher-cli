package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPointerFor(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "/metadata/resourceVersion", PointerFor([]string{"metadata", "resourceVersion"}))
	assert.Equal(t, "/a~1b", PointerFor([]string{"a/b"}))
	assert.Equal(t, "", PointerFor(nil), "no keys addresses the whole document")
}

func TestEscapePointerToken(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "app.kubernetes.io~1name", escapePointerToken("app.kubernetes.io/name"))
	assert.Equal(t, "a~0b", escapePointerToken("a~b"))
	assert.Equal(t, "a~0~1b", escapePointerToken("a~/b"))
	assert.Equal(t, "plain", escapePointerToken("plain"))
}

func TestUnescapePointerToken(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "app.kubernetes.io/name", unescapePointerToken("app.kubernetes.io~1name"))
	assert.Equal(t, "a~b", unescapePointerToken("a~0b"))
	assert.Equal(t, "a~/b", unescapePointerToken("a~0~1b"))
	assert.Equal(t, "plain", unescapePointerToken("plain"))
}

func TestEscapePointerTokenRoundTrip(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"plain",
		"app.kubernetes.io/name",
		"a~b",
		"a~/b",
		"~1",
		"~0",
		"//",
		"~~",
	} {
		assert.Equal(t, s, unescapePointerToken(escapePointerToken(s)), "round trip for %q", s)
	}
}

func TestSplitPointer(t *testing.T) {
	t.Parallel()
	assert.Nil(t, SplitPointer(""), "the empty pointer addresses the whole document")
	assert.Equal(t, []string{""}, SplitPointer("/"), "\"/\" addresses the top-level empty-string key")
	assert.Equal(t, []string{"spec"}, SplitPointer("/spec"))
	assert.Equal(t, []string{"metadata", "labels", "app.kubernetes.io/name"},
		SplitPointer("/metadata/labels/app.kubernetes.io~1name"))
	assert.Equal(t, []string{"a", "0", "b"}, SplitPointer("/a/0/b"))
}
