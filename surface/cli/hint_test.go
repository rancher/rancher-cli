package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestFirstHintFirstMatchWins pins the ordering contract of firstHint:
// when two entries both match the input, the first entry's hint is
// returned, not the second's. A no-match case confirms the fallback.
func TestFirstHintFirstMatchWins(t *testing.T) {
	t.Parallel()

	entries := []hintEntry[string]{
		{match: func(string) bool { return true }, hint: func(string) string { return "first" }},
		{match: func(string) bool { return true }, hint: func(string) string { return "second" }},
	}
	assert.Equal(t, "first", firstHint(entries, "any", "fallback"),
		"first matching entry must win when both entries match")

	noMatch := []hintEntry[string]{
		{match: func(string) bool { return false }, hint: func(string) string { return "never" }},
	}
	assert.Equal(t, "fallback", firstHint(noMatch, "any", "fallback"),
		"fallback must be returned when no entry matches")
}
