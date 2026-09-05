package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEachConditionStopsWhenCallbackReturnsFalse(t *testing.T) {
	t.Parallel()
	obj := map[string]any{"status": map[string]any{"conditions": []any{
		map[string]any{"type": "A", "status": "True"},
		"malformed entry - skipped, not counted",
		map[string]any{"type": "B", "status": "False", "reason": "r", "message": "m"},
		map[string]any{"type": "C", "status": "True"},
	}}}

	var visited []string
	EachCondition(obj, func(typ, status, reason, message string) bool {
		visited = append(visited, typ)
		return typ != "B"
	})
	assert.Equal(t, []string{"A", "B"}, visited, "walk stops where the callback returns false; malformed entries are skipped")

	visited = nil
	EachCondition(obj, func(typ, _, _, _ string) bool {
		visited = append(visited, typ)
		return true
	})
	assert.Equal(t, []string{"A", "B", "C"}, visited, "true walks everything")
}
