package completion

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestItemListNames asserts metadata.name is read in list order and items
// without one are skipped.
func TestItemListNames(t *testing.T) {
	t.Parallel()
	l := ItemList{Items: []map[string]any{
		{"metadata": map[string]any{"name": "b"}},
		{"metadata": map[string]any{}},
		{"kind": "X"},
		{"metadata": map[string]any{"name": 3}},
		{"metadata": map[string]any{"name": "a"}},
	}}
	assert.Equal(t, []string{"b", "a"}, l.Names())
	assert.Empty(t, ItemList{}.Names())
}

// TestDedupe asserts the result is sorted and distinct and the input is
// left alone.
func TestDedupe(t *testing.T) {
	t.Parallel()
	in := []string{"b", "a", "b", "c", "a"}
	got := Dedupe(in)
	assert.Equal(t, []string{"a", "b", "c"}, got)
	assert.Equal(t, []string{"b", "a", "b", "c", "a"}, in)
	assert.Empty(t, Dedupe(nil))
}
