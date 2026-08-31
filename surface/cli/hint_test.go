package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestFirstMatchFirstEntryWins pins the ordering contract of firstMatch:
// when two entries both match the input, the first entry's detail/hint is
// returned, not the second's. A no-match case confirms ok is false.
func TestFirstMatchFirstEntryWins(t *testing.T) {
	t.Parallel()

	entries := []hintEntry[string]{
		{match: func(string) bool { return true }, detail: func(string) string { return "first" }},
		{match: func(string) bool { return true }, detail: func(string) string { return "second" }},
	}
	detail, hint, ok := firstMatch(entries, "any")
	assert.True(t, ok)
	assert.Equal(t, "first", detail, "first matching entry must win when both entries match")
	assert.Nil(t, hint, "an entry with no hint override leaves hint nil")

	noMatch := []hintEntry[string]{
		{match: func(string) bool { return false }, detail: func(string) string { return "never" }},
	}
	_, _, ok = firstMatch(noMatch, "any")
	assert.False(t, ok, "ok must be false when no entry matches")
}
