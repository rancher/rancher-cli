package kinds

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func namedObj(name string, extra map[string]any) map[string]any {
	obj := map[string]any{"metadata": map[string]any{"name": name}}
	maps.Copy(obj, extra)
	return obj
}

// TestRoleTemplateLocked asserts only the JSON boolean true counts as
// locked: false, a missing field, the string "true", a number and a nil
// object do not.
func TestRoleTemplateLocked(t *testing.T) {
	t.Parallel()
	assert.True(t, RoleTemplateLocked(map[string]any{"locked": true}))
	assert.False(t, RoleTemplateLocked(map[string]any{"locked": false}))
	assert.False(t, RoleTemplateLocked(map[string]any{}))
	assert.False(t, RoleTemplateLocked(map[string]any{"locked": "true"}))
	assert.False(t, RoleTemplateLocked(map[string]any{"locked": 1}))
	assert.False(t, RoleTemplateLocked(nil))
}

// TestIndexByName asserts the index holds each named object once, the first
// of a repeated name, and skips an object with no name.
func TestIndexByName(t *testing.T) {
	t.Parallel()
	first := namedObj("a", map[string]any{"n": 1})
	index := indexByName([]map[string]any{first, namedObj("a", map[string]any{"n": 2}), namedObj("b", nil), {"spec": 1}, namedObj("", nil)})
	assert.Len(t, index, 2)
	assert.Equal(t, 1, index["a"]["n"])
	assert.Contains(t, index, "b")
}

// TestGlobalRoleInheritedTemplates asserts the names come from the global
// role's own inheritedClusterRoles, and that a missing field, a value that is
// not a list and an entry that is not a string name nothing.
func TestGlobalRoleInheritedTemplates(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"a", "b"}, globalRoleInheritedTemplates(map[string]any{"inheritedClusterRoles": []any{"a", "b"}}))
	assert.Equal(t, []string{"a"}, globalRoleInheritedTemplates(map[string]any{"inheritedClusterRoles": []any{"a", 3, nil}}))
	assert.Empty(t, globalRoleInheritedTemplates(map[string]any{"inheritedClusterRoles": []any{}}))
	assert.Empty(t, globalRoleInheritedTemplates(map[string]any{"inheritedClusterRoles": "a"}))
	assert.Empty(t, globalRoleInheritedTemplates(map[string]any{}))
	assert.Empty(t, globalRoleInheritedTemplates(nil))
}

// lockedRulesTemplates indexes role templates of every state the rule reads.
// parent-of-locked is unlocked and inherits a locked role template through
// roleTemplateNames, which Rancher does not follow.
func lockedRulesTemplates() map[string]map[string]any {
	return indexByName([]map[string]any{
		namedObj("cluster-open", map[string]any{"context": "cluster"}),
		namedObj("cluster-hidden", map[string]any{"context": "cluster", "hidden": true}),
		namedObj("cluster-false", map[string]any{"context": "cluster", "locked": false}),
		namedObj("cluster-locked", map[string]any{"context": "cluster", "locked": true}),
		namedObj("project-locked", map[string]any{"context": "project", "locked": true}),
		namedObj("orphan-locked", map[string]any{"locked": true}),
		namedObj("string-locked", map[string]any{"context": "cluster", "locked": "true"}),
		namedObj("cluster-parent-of-locked", map[string]any{"context": "cluster", "roleTemplateNames": []any{"cluster-locked"}}),
	})
}

// TestGlobalRoleInheritsLocked asserts the rule over each shape of a global
// role: a role template named by the global role that is locked, locked with
// another context or none, or missing from the index leaves the role out;
// an open, hidden, false-locked, string-locked and unlocked parent of a
// locked role template does not; the field is read by name only, never
// through a role template's own roleTemplateNames, and no other field of the
// global role counts.
func TestGlobalRoleInheritsLocked(t *testing.T) {
	t.Parallel()
	templates := lockedRulesTemplates()
	tests := []struct {
		name string
		role map[string]any
		want bool
	}{
		{name: "no field", role: map[string]any{}},
		{name: "empty list", role: map[string]any{"inheritedClusterRoles": []any{}}},
		{name: "not a list", role: map[string]any{"inheritedClusterRoles": "cluster-locked"}},
		{name: "open", role: map[string]any{"inheritedClusterRoles": []any{"cluster-open"}}},
		{name: "hidden", role: map[string]any{"inheritedClusterRoles": []any{"cluster-hidden"}}},
		{name: "locked false", role: map[string]any{"inheritedClusterRoles": []any{"cluster-false"}}},
		{name: "locked string", role: map[string]any{"inheritedClusterRoles": []any{"string-locked"}}},
		{name: "parent of locked", role: map[string]any{"inheritedClusterRoles": []any{"cluster-parent-of-locked"}}},
		{name: "other field", role: map[string]any{"roleTemplateNames": []any{"cluster-locked"}}},
		{name: "locked", role: map[string]any{"inheritedClusterRoles": []any{"cluster-locked"}}, want: true},
		{name: "locked of another context", role: map[string]any{"inheritedClusterRoles": []any{"project-locked"}}, want: true},
		{name: "locked with no context", role: map[string]any{"inheritedClusterRoles": []any{"orphan-locked"}}, want: true},
		{name: "open and locked", role: map[string]any{"inheritedClusterRoles": []any{"cluster-open", "cluster-locked"}}, want: true},
		{name: "missing", role: map[string]any{"inheritedClusterRoles": []any{"no-such-template"}}, want: true},
		{name: "open and missing", role: map[string]any{"inheritedClusterRoles": []any{"cluster-open", "no-such-template"}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, globalRoleInheritsLocked(tt.role, templates))
		})
	}
	assert.True(t, globalRoleInheritsLocked(map[string]any{"inheritedClusterRoles": []any{"cluster-open"}}, nil), "an empty index lists no role template")
}

// TestBindableGlobalRoles asserts the role templates are listed once and
// only when some global role names one, the roles left out are those
// globalRoleInheritsLocked refuses, and a failing listing keeps the roles
// that name none and returns the error.
func TestBindableGlobalRoles(t *testing.T) {
	t.Parallel()
	open := namedObj("open", nil)
	empty := namedObj("empty", map[string]any{"inheritedClusterRoles": []any{}})
	fine := namedObj("fine", map[string]any{"inheritedClusterRoles": []any{"cluster-open"}})
	locked := namedObj("locked", map[string]any{"inheritedClusterRoles": []any{"cluster-locked"}})
	missing := namedObj("missing", map[string]any{"inheritedClusterRoles": []any{"no-such-template"}})
	templates := slices.Collect(maps.Values(lockedRulesTemplates()))
	t.Run("no role names a template", func(t *testing.T) {
		t.Parallel()
		got, err := BindableGlobalRoles([]map[string]any{open, empty}, func() ([]map[string]any, error) {
			require.Fail(t, "the role templates are listed only when a role names one")
			return nil, nil
		})
		require.NoError(t, err)
		assert.Equal(t, []map[string]any{open, empty}, got)
	})
	t.Run("filters by the role templates", func(t *testing.T) {
		t.Parallel()
		calls := 0
		got, err := BindableGlobalRoles([]map[string]any{open, fine, locked, missing, empty}, func() ([]map[string]any, error) {
			calls++
			return templates, nil
		})
		require.NoError(t, err)
		assert.Equal(t, []map[string]any{open, fine, empty}, got)
		assert.Equal(t, 1, calls)
	})
	t.Run("an empty role template list leaves out every role naming one", func(t *testing.T) {
		t.Parallel()
		got, err := BindableGlobalRoles([]map[string]any{open, fine, locked, empty}, func() ([]map[string]any, error) { return nil, nil })
		require.NoError(t, err)
		assert.Equal(t, []map[string]any{open, empty}, got)
	})
	t.Run("a failing listing keeps the roles naming none", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("boom")
		got, err := BindableGlobalRoles([]map[string]any{open, fine, locked, empty}, func() ([]map[string]any, error) { return nil, boom })
		require.ErrorIs(t, err, boom)
		assert.Equal(t, []map[string]any{open, empty}, got)
	})
}
