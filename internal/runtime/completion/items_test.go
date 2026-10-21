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

// values returns the value list of an IDsWithDisplayNames answer.
func values(v []string, _ map[string]string) []string { return v }

// TestIDsWithUniqueDisplayNames asserts the value list of
// IDsWithDisplayNames: every id is offered, a display name only when it
// appears on one row and is not itself an id, each value once, in list
// order; an item without metadata is skipped whole and a repeated row
// offers its id once while its display name counts twice.
func TestIDsWithUniqueDisplayNames(t *testing.T) {
	t.Parallel()
	cluster := func(id, display string) map[string]any {
		return map[string]any{"metadata": map[string]any{"name": id}, "spec": map[string]any{"displayName": display}}
	}
	items := []map[string]any{
		cluster("c-aaa11", "prod"),
		cluster("local", "local"),
		cluster("c-bbb22", "dup"),
		cluster("c-ccc33", "dup"),
		cluster("c-ddd44", "c-aaa11"),
		{"metadata": map[string]any{}},
		{"spec": map[string]any{"displayName": "orphan"}},
		cluster("c-eee55", ""),
	}
	assert.Equal(t, []string{"c-aaa11", "prod", "local", "c-bbb22", "c-ccc33", "c-ddd44", "c-eee55"}, values(IDsWithDisplayNames(items)))
	repeated := []map[string]any{cluster("c-aaa11", "prod"), cluster("c-aaa11", "prod")}
	assert.Equal(t, []string{"c-aaa11"}, values(IDsWithDisplayNames(repeated)))
	assert.Empty(t, values(IDsWithDisplayNames(nil)))
}

// TestIDsWithDisplayNames asserts each id's display name: shared and
// colliding display names still describe their ids, an id without a
// display name has no entry, a repeated id keeps its first non-empty
// display name, and an item without metadata is skipped whole.
func TestIDsWithDisplayNames(t *testing.T) {
	t.Parallel()
	cluster := func(id, display string) map[string]any {
		return map[string]any{"metadata": map[string]any{"name": id}, "spec": map[string]any{"displayName": display, "description": "not the display name"}}
	}
	items := []map[string]any{
		cluster("c-aaa11", "prod"),
		cluster("local", "local"),
		cluster("c-bbb22", "dup"),
		cluster("c-ccc33", "dup"),
		cluster("c-ddd44", "c-aaa11"),
		{"metadata": map[string]any{}},
		{"spec": map[string]any{"displayName": "orphan"}},
		cluster("c-eee55", ""),
		cluster("c-eee55", "late"),
		cluster("c-fff66", "first"),
		cluster("c-fff66", "second"),
	}
	_, displayNames := IDsWithDisplayNames(items)
	assert.Equal(t, map[string]string{
		"c-aaa11": "prod",
		"local":   "local",
		"c-bbb22": "dup",
		"c-ccc33": "dup",
		"c-ddd44": "c-aaa11",
		"c-eee55": "late",
		"c-fff66": "first",
	}, displayNames)

	vals, displayNames := IDsWithDisplayNames(nil)
	assert.Empty(t, vals)
	assert.Empty(t, displayNames)
}

// TestTailFlag asserts the space form, the equals form (empty value
// included), an absent flag, a flag that is the last token, that a
// prefix of the name is not the flag, and that a flag typed more than
// once takes its last spelling, a trailing bare one included.
func TestTailFlag(t *testing.T) {
	t.Parallel()
	tail := []string{"get", "pod", "--cluster", "c-x", "--namespace=default", "--project=", "--type"}
	assert.Equal(t, "c-x", TailFlag(tail, "cluster"))
	assert.Equal(t, "default", TailFlag(tail, "namespace"))
	assert.Empty(t, TailFlag(tail, "project"))
	assert.Empty(t, TailFlag(tail, "type"))
	assert.Empty(t, TailFlag(tail, "node"))
	assert.Empty(t, TailFlag([]string{"--clusters", "x"}, "cluster"))
	assert.Equal(t, "--namespace", TailFlag([]string{"--cluster", "--namespace"}, "cluster"))
	assert.Equal(t, "group", TailFlag([]string{"--type", "user", "--type", "group"}, "type"))
	assert.Equal(t, "user", TailFlag([]string{"--type", "group", "--type", "user"}, "type"))
	assert.Equal(t, "user", TailFlag([]string{"--type=group", "--type", "user"}, "type"))
	assert.Equal(t, "group", TailFlag([]string{"--type", "user", "--type=group"}, "type"))
	assert.Empty(t, TailFlag([]string{"--type", "group", "--type"}, "type"), "the last spelling has no value yet")
}
