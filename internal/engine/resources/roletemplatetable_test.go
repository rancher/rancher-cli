package resources

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestRoleTemplateTableShowsTheHiddenField asserts the table of a list of role
// templates has a HIDDEN column after CONTEXT and DISPLAY NAME that renders
// the hidden field as true or false, false for a role template that has no
// hidden field, and that -o wide adds no other column.
func TestRoleTemplateTableShowsTheHiddenField(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "roletemplate")
	item := func(name, context string, hidden any) unstructured.Unstructured {
		obj := map[string]any{
			"metadata":    map[string]any{"name": name},
			"context":     context,
			"displayName": "Role " + name,
		}
		if hidden != nil {
			obj["hidden"] = hidden
		}
		return unstructured.Unstructured{Object: obj}
	}
	tab := newTable(k, []unstructured.Unstructured{
		item("admin", "project", true),
		item("project-member", "project", false),
		item("legacy", "cluster", nil),
	}, false)
	assert.Equal(t, []string{"NAME", "CONTEXT", "DISPLAY NAME", "HIDDEN", "AGE"}, tab.Columns())
	require.Len(t, tab.Rows(), 3)
	assert.Equal(t, []string{"admin", "project", "Role admin", "true", ""}, tab.Rows()[0])
	assert.Equal(t, []string{"project-member", "project", "Role project-member", "false", ""}, tab.Rows()[1])
	assert.Equal(t, []string{"legacy", "cluster", "Role legacy", "false", ""}, tab.Rows()[2])
	assert.Equal(t, tab.Columns(), tab.WideColumns())
}
