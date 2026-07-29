package resources

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestTableRowsRenderBoolLeaves pins the column renderer's type coverage:
// string leaves render verbatim, bool leaves render "true"/"false" (the
// nodedriver row's ACTIVE/BUILTIN columns are bools in the served v3
// NodeDriver), an ABSENT field in a Bool-marked column renders "false" —
// rancher's NodeDriverSpec.Builtin is `json:"builtin,omitempty"`, so a
// non-builtin driver omits the field entirely — while an absent field in an
// unmarked (string-typed) column stays blank, and any other leaf type
// (int64, array) still renders blank — the token/kubeconfig rows
// deliberately omit such columns.
func TestTableRowsRenderBoolLeaves(t *testing.T) {
	t.Parallel()
	k := kinds.Kind{Name: "widget", Columns: []kinds.Column{
		{Header: "DISPLAY", Path: []string{"spec", "displayName"}},
		{Header: "ACTIVE", Path: []string{"spec", "active"}, Bool: true},
		{Header: "BUILTIN", Path: []string{"spec", "builtin"}, Bool: true},
		{Header: "TTL", Path: []string{"spec", "ttl"}},
		{Header: "MISSING", Path: []string{"spec", "absent"}},
	}}
	builtin := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "w1"},
		"spec": map[string]any{
			"displayName": "Widget One",
			"active":      true,
			"builtin":     true,
			"ttl":         int64(300),
		},
	}}
	inactive := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "w2"},
		"spec": map[string]any{
			"displayName": "Widget Two",
			"active":      false,
			"builtin":     false,
		},
	}}
	// The omitted-field wire shape: a non-builtin driver has NO "builtin"
	// key at all (omitempty), yet BUILTIN must render "false", not blank.
	// MISSING (string-typed, also absent) must stay blank.
	omitted := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "w3"},
		"spec": map[string]any{
			"displayName": "Widget Three",
			"active":      true,
		},
	}}
	tab := newTable(k, []unstructured.Unstructured{builtin, inactive, omitted}, false)
	require.Len(t, tab.Rows(), 3)
	assert.Equal(t, []string{"w1", "Widget One", "true", "true", "", ""}, tab.Rows()[0])
	assert.Equal(t, []string{"w2", "Widget Two", "false", "false", "", ""}, tab.Rows()[1])
	assert.Equal(t, []string{"w3", "Widget Three", "true", "false", "", ""}, tab.Rows()[2])
}

func TestColumnStringConditionDerived(t *testing.T) {
	t.Parallel()
	col := kinds.Column{Header: "READY", Condition: "Ready"}
	obj := func(status string) map[string]any {
		return map[string]any{"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Provisioned", "status": "True"},
			map[string]any{"type": "Ready", "status": status},
		}}}
	}
	assert.Equal(t, "Ready", columnString(obj("True"), col))
	assert.Equal(t, "NotReady", columnString(obj("False"), col))
	assert.Equal(t, "Unknown", columnString(obj("Unknown"), col))
	assert.Equal(t, "", columnString(map[string]any{}, col), "absent condition renders blank, never fabricated")
}

// TestTableMarshalJSONSingleInvariant drives the real output.Render path (not
// a direct MarshalJSON call) so a regression here fails the same way it would
// in production: get.go always passes exactly 1 item for a named get, so this
// is unreached today, but a single result with 0 or 2+ items must error
// loudly rather than silently render null or drop items.
func TestTableMarshalJSONSingleInvariant(t *testing.T) {
	k := mustKind(t, "cluster")
	tests := []struct {
		name  string
		items []unstructured.Unstructured
	}{
		{"zero items", nil},
		{"two items", []unstructured.Unstructured{
			clusterObj("a", "a", "active"),
			clusterObj("b", "b", "active"),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			env := output.NewEnvelope("Cluster", newTable(k, tt.items, true))
			err := output.Render(&buf, "json", env)
			require.Error(t, err)
			assert.Contains(t, err.Error(), fmt.Sprintf("got %d", len(tt.items)))
		})
	}
}
