package resources

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
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
	assert.Equal(t, "", columnString(obj(""), col), "degenerate statusless entry renders blank")
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

// TestTableMarshalJSONRedactsKindPaths verifies that a kind's RedactPaths are
// absent from -o json output. The test decodes the marshaled JSON rather than
// string-matching, so whitespace or key ordering can never cause a false pass.
func TestTableMarshalJSONRedactsKindPaths(t *testing.T) {
	t.Parallel()
	k := kinds.Kind{
		Name:        "widget",
		K8sKind:     "Widget",
		RedactPaths: [][]string{{"secret"}, {"nested", "cred"}},
	}
	obj := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "w1"},
		"public":   "visible",
		"secret":   "must-be-gone",
		"nested":   map[string]any{"cred": "also-gone", "safe": "keep"},
	}}
	tab := newTable(k, []unstructured.Unstructured{obj}, true)
	b, err := tab.MarshalJSON()
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(b, &decoded))
	assert.Equal(t, "visible", decoded["public"], "non-redacted field must be present")
	assert.NotContains(t, decoded, "secret", "RedactPaths root field must be absent")
	nested, _ := decoded["nested"].(map[string]any)
	require.NotNil(t, nested, "parent of a nested redact path must still be present")
	assert.NotContains(t, nested, "cred", "nested redact field must be absent")
	assert.Equal(t, "keep", nested["safe"], "sibling of redacted nested field must be present")
}

// TestTableMarshalJSONRedactsListItems verifies that redaction applies to
// every item in a list result, not just the first.
func TestTableMarshalJSONRedactsListItems(t *testing.T) {
	t.Parallel()
	k := kinds.Kind{
		Name:        "widget",
		K8sKind:     "Widget",
		RedactPaths: [][]string{{"token"}},
	}
	items := []unstructured.Unstructured{
		{Object: map[string]any{"metadata": map[string]any{"name": "a"}, "token": "cred-a"}},
		{Object: map[string]any{"metadata": map[string]any{"name": "b"}, "token": "cred-b"}},
	}
	tab := newTable(k, items, false)
	b, err := tab.MarshalJSON()
	require.NoError(t, err)

	var decoded []map[string]any
	require.NoError(t, json.Unmarshal(b, &decoded))
	require.Len(t, decoded, 2)
	assert.NotContains(t, decoded[0], "token", "token must be absent from item 0")
	assert.NotContains(t, decoded[1], "token", "token must be absent from item 1")
}

// TestTableMarshalJSONDoesNotMutateOriginal verifies that engine.ScrubPaths does not
// mutate the original unstructured object (important: a command may hold the
// object in memory after serialization).
func TestTableMarshalJSONDoesNotMutateOriginal(t *testing.T) {
	t.Parallel()
	k := kinds.Kind{
		Name:        "widget",
		K8sKind:     "Widget",
		RedactPaths: [][]string{{"secret"}},
	}
	obj := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "w1"},
		"secret":   "must-survive-in-original",
	}}
	tab := newTable(k, []unstructured.Unstructured{obj}, true)
	_, err := tab.MarshalJSON()
	require.NoError(t, err)
	assert.Equal(t, "must-survive-in-original", obj.Object["secret"],
		"the original unstructured object must not be mutated by engine.ScrubPaths")
}

// TestRedactKindPathsBlanksDiffEntries verifies that redactKindPaths replaces
// From/To with "<redacted>" for diff entries whose path matches a RedactPaths
// entry, and leaves unrelated entries intact.
func TestRedactKindPathsBlanksDiffEntries(t *testing.T) {
	t.Parallel()
	diff := []safety.DiffEntry{
		{Path: "/token", From: "old-cred", To: "new-cred"},
		{Path: "/token/sub", From: "sub-old", To: "sub-new"},
		{Path: "/description", From: "old-desc", To: "new-desc"},
		{Path: "/tokenExtra", From: "keep-old", To: "keep-new"}, // prefix match must not fire
	}
	redactKindPaths(diff, [][]string{{"token"}})

	assert.Equal(t, "<redacted>", diff[0].From, "/token From must be redacted")
	assert.Equal(t, "<redacted>", diff[0].To, "/token To must be redacted")
	assert.Equal(t, "<redacted>", diff[1].From, "/token/sub From must be redacted")
	assert.Equal(t, "<redacted>", diff[1].To, "/token/sub To must be redacted")
	assert.Equal(t, "old-desc", diff[2].From, "/description must be untouched")
	assert.Equal(t, "keep-old", diff[3].From, "/tokenExtra must not match /token prefix")
}
