package resources

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestTableRowsRenderBoolLeaves asserts the column renderer's typed leaf ladder
// (NestedString → NestedBool → NestedInt64 → NestedFloat64): string leaves
// render verbatim, bool leaves render "true"/"false" (the nodedriver row's
// ACTIVE/BUILTIN columns are bools in the served v3 NodeDriver), an ABSENT
// field in a Bool-marked column renders "false" - rancher's
// NodeDriverSpec.Builtin is `json:"builtin,omitempty"`, so a non-builtin
// driver omits the field entirely - an absent field in an unmarked
// (string-typed) column stays blank, and - the inversion from the renderer's
// former string/bool-only behavior - a present int64 leaf (TTL) now renders
// its decimal value instead of blanking.
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
	// AGE is a renderer built-in, always last; none of these fixtures set
	// metadata.creationTimestamp, so it renders blank.
	assert.Equal(t, []string{"w1", "Widget One", "true", "true", "300", "", ""}, tab.Rows()[0])
	assert.Equal(t, []string{"w2", "Widget Two", "false", "false", "", "", ""}, tab.Rows()[1])
	assert.Equal(t, []string{"w3", "Widget Three", "true", "false", "", "", ""}, tab.Rows()[2])
}

// TestPathCellLadder table-drives the typed leaf ladder across every leaf
// type and both Path and AltPaths, plus the terminal absent-everywhere
// fallbacks (Bool marker, Default, blank).
func TestPathCellLadder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		col  kinds.Column
		obj  map[string]any
		want string
	}{
		{
			name: "string at Path renders verbatim",
			col:  kinds.Column{Path: []string{"a"}},
			obj:  map[string]any{"a": "hello"},
			want: "hello",
		},
		{
			name: "empty string at Path falls through to AltPath",
			col:  kinds.Column{Path: []string{"a"}, AltPaths: [][]string{{"b"}}},
			obj:  map[string]any{"a": "", "b": "fallback"},
			want: "fallback",
		},
		{
			name: "bool true at Path renders true",
			col:  kinds.Column{Path: []string{"a"}},
			obj:  map[string]any{"a": true},
			want: "true",
		},
		{
			name: "bool false at Path renders false, not blank",
			col:  kinds.Column{Path: []string{"a"}},
			obj:  map[string]any{"a": false},
			want: "false",
		},
		{
			name: "present int64 zero renders 0, never blank",
			col:  kinds.Column{Path: []string{"a"}},
			obj:  map[string]any{"a": int64(0)},
			want: "0",
		},
		{
			name: "present int64 nonzero renders decimal",
			col:  kinds.Column{Path: []string{"a"}},
			obj:  map[string]any{"a": int64(42)},
			want: "42",
		},
		{
			name: "present float64 renders shortest round-trip",
			col:  kinds.Column{Path: []string{"a"}},
			obj:  map[string]any{"a": 3.5},
			want: "3.5",
		},
		{
			name: "present float64 zero renders 0, never blank",
			col:  kinds.Column{Path: []string{"a"}},
			obj:  map[string]any{"a": 0.0},
			want: "0",
		},
		{
			name: "extreme-magnitude float64 may emit exponent form",
			col:  kinds.Column{Path: []string{"a"}},
			obj:  map[string]any{"a": 1e21},
			want: "1e+21",
		},
		{
			name: "int64 at AltPath renders when Path is absent",
			col:  kinds.Column{Path: []string{"missing"}, AltPaths: [][]string{{"a"}}},
			obj:  map[string]any{"a": int64(7)},
			want: "7",
		},
		{
			name: "Path's bool wins before any AltPath is tried (ordering inversion)",
			col:  kinds.Column{Path: []string{"a"}, AltPaths: [][]string{{"b"}}},
			obj:  map[string]any{"a": true, "b": "should-not-be-used"},
			want: "true",
		},
		{
			// Models the feature VALUE merge: a bool-typed Path present and
			// non-absent (even when false) always wins over the AltPath.
			name: "bool-typed Path false wins over AltPath's bool default",
			col:  kinds.Column{Path: []string{"spec", "value"}, AltPaths: [][]string{{"status", "default"}}},
			obj:  map[string]any{"spec": map[string]any{"value": false}, "status": map[string]any{"default": true}},
			want: "false",
		},
		{
			// Models the feature VALUE merge in the other direction: Path
			// absent (an omitempty *bool with no value set) falls through to
			// the AltPath's bool.
			name: "bool-typed Path absent falls through to AltPath's bool",
			col:  kinds.Column{Path: []string{"spec", "value"}, AltPaths: [][]string{{"status", "default"}}},
			obj:  map[string]any{"spec": map[string]any{}, "status": map[string]any{"default": true}},
			want: "true",
		},
		{
			name: "absent everywhere with Bool marker renders false",
			col:  kinds.Column{Path: []string{"missing"}, Bool: true},
			obj:  map[string]any{},
			want: "false",
		},
		{
			name: "absent everywhere with Default renders Default",
			col:  kinds.Column{Path: []string{"missing"}, Default: "0"},
			obj:  map[string]any{},
			want: "0",
		},
		{
			name: "absent everywhere with neither renders blank",
			col:  kinds.Column{Path: []string{"missing"}},
			obj:  map[string]any{},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, pathCell(tt.obj, tt.col))
		})
	}
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
			assert.ErrorContains(t, err, fmt.Sprintf("got %d", len(tt.items)))
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

// TestTableRowsRenderJoinAndDefault covers the two column axes the user
// kind row needs: Join renders a string-array field comma-joined in server
// order (principalIds - every element, no ordering assumption), and
// Default renders when the field is absent (User.enabled is an omitempty
// *bool whose absence means enabled).
func TestTableRowsRenderJoinAndDefault(t *testing.T) {
	t.Parallel()
	k := kinds.Kind{Name: "user", Columns: []kinds.Column{
		{Header: "ENABLED", Path: []string{"enabled"}, Default: "true"},
		{Header: "PRINCIPALS", Path: []string{"principalIds"}, Join: true},
	}}
	seeded := unstructured.Unstructured{Object: map[string]any{
		"metadata":     map[string]any{"name": "m-1"},
		"enabled":      true,
		"principalIds": []any{"local://m-1"},
	}}
	disabled := unstructured.Unstructured{Object: map[string]any{
		"metadata":     map[string]any{"name": "m-2"},
		"enabled":      false,
		"principalIds": []any{"okta_user://abc", "local://m-2"},
	}}
	// System accounts omit enabled entirely (absence means enabled) and
	// lead with the system:// principal.
	system := unstructured.Unstructured{Object: map[string]any{
		"metadata":     map[string]any{"name": "u-1"},
		"principalIds": []any{"system://local", "local://u-1"},
	}}
	// No principals at all: the PRINCIPAL cell stays blank, never invented.
	bare := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "u-2"},
	}}
	tab := newTable(k, []unstructured.Unstructured{seeded, disabled, system, bare}, false)
	require.Len(t, tab.Rows(), 4)
	assert.Equal(t, []string{"m-1", "true", "local://m-1", ""}, tab.Rows()[0])
	assert.Equal(t, []string{"m-2", "false", "okta_user://abc,local://m-2", ""}, tab.Rows()[1])
	assert.Equal(t, []string{"u-1", "true", "system://local,local://u-1", ""}, tab.Rows()[2])
	assert.Equal(t, []string{"u-2", "true", "", ""}, tab.Rows()[3])
}

// TestTableRowsRenderSettingProvenance asserts Setting's CUSTOMIZED/SOURCE
// columns against the real wire shape: value/default/customized/source sit
// at the object root (Setting is a flat CRD, not spec/status), and both
// fields are always emitted (never omitempty), verified live against a
// v2.14.3 Rancher server on 2026-08-18.
func TestTableRowsRenderSettingProvenance(t *testing.T) {
	t.Parallel()
	k, ok := kinds.Lookup("setting")
	require.True(t, ok)
	stock := unstructured.Unstructured{Object: map[string]any{
		"metadata":   map[string]any{"name": "agent-tls-mode"},
		"value":      "system-store",
		"default":    "strict",
		"customized": false,
		"source":     "",
	}}
	overridden := unstructured.Unstructured{Object: map[string]any{
		"metadata":   map[string]any{"name": "server-url"},
		"value":      "https://rancher.peterm.dev",
		"default":    "",
		"customized": true,
		"source":     "env",
	}}
	tab := newTable(k, []unstructured.Unstructured{stock, overridden}, false)
	require.Len(t, tab.Rows(), 2)
	assert.Equal(t, []string{"agent-tls-mode", "false", "", ""}, tab.Rows()[0])
	assert.Equal(t, []string{"server-url", "true", "env", ""}, tab.Rows()[1])
}

// TestTableRowsRenderFeatureBoolStatus asserts Feature's VALUE/DEFAULT/DYNAMIC/
// LOCKED columns: all are plain status bools (or, for VALUE, the merged
// spec.value/status.default fallback) verified live against a v2.14.3
// Rancher server on 2026-08-18. lockedValue is an omitempty *bool - absent
// (unlocked) must render blank, never "false", since a locked-false feature
// is a real, different state. VALUE is asserted both directions: spec.value
// absent falls through to status.default, and spec.value present (even
// false) wins over status.default.
func TestTableRowsRenderFeatureBoolStatus(t *testing.T) {
	t.Parallel()
	k, ok := kinds.Lookup("feature")
	require.True(t, ok)
	unlocked := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "cluster-autoscaling"},
		"status": map[string]any{
			"default":     true,
			"dynamic":     true,
			"description": "Enable Rancher cluster-autoscaler support",
		},
	}}
	locked := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "some-locked-feature"},
		"status": map[string]any{
			"default":     false,
			"dynamic":     false,
			"lockedValue": false,
			"description": "Locked off",
		},
	}}
	overridden := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "explicitly-off"},
		"spec":     map[string]any{"value": false},
		"status": map[string]any{
			"default":     true,
			"dynamic":     true,
			"description": "Explicitly disabled despite a true default",
		},
	}}
	tab := newTable(k, []unstructured.Unstructured{unlocked, locked, overridden}, false)
	require.Len(t, tab.Rows(), 3)
	assert.Equal(t, []string{"cluster-autoscaling", "true", "true", "true", "", "", "Enable Rancher cluster-autoscaler support", ""}, tab.Rows()[0])
	assert.Equal(t, []string{"some-locked-feature", "false", "false", "false", "false", "", "Locked off", ""}, tab.Rows()[1])
	assert.Equal(t, []string{"explicitly-off", "false", "true", "true", "", "", "Explicitly disabled despite a true default", ""}, tab.Rows()[2])
}

// TestTableRowsRenderFeaturePrimeOnlyColumn asserts PRIME-ONLY: it renders the
// wire's status.prime classification (true/false), and blank - never
// "false" - when an older server omits the field: absent is not false,
// nothing is fabricated.
func TestTableRowsRenderFeaturePrimeOnlyColumn(t *testing.T) {
	t.Parallel()
	k, ok := kinds.Lookup("feature")
	require.True(t, ok)
	primeTrue := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "cluster-autoscaling"},
		"status":   map[string]any{"default": false, "prime": true},
	}}
	primeFalse := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "some-feature"},
		"status":   map[string]any{"default": true, "prime": false},
	}}
	primeAbsent := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "older-server-feature"},
		"status":   map[string]any{"default": true},
	}}
	tab := newTable(k, []unstructured.Unstructured{primeTrue, primeFalse, primeAbsent}, false)
	require.Len(t, tab.Rows(), 3)
	primeOnlyIdx := slices.Index(tab.Columns(), "PRIME-ONLY")
	require.GreaterOrEqual(t, primeOnlyIdx, 0)
	assert.Equal(t, "true", tab.Rows()[0][primeOnlyIdx])
	assert.Equal(t, "false", tab.Rows()[1][primeOnlyIdx])
	assert.Equal(t, "", tab.Rows()[2][primeOnlyIdx], "absent status.prime must render blank, never false")
}

// TestTableRowsRenderCount asserts the Count axis: len() of the map/slice at
// the path, absent → blank, present-but-empty → "0" (a real count, never
// fabricated).
func TestTableRowsRenderCount(t *testing.T) {
	t.Parallel()
	k := kinds.Kind{Name: "configmap", Columns: []kinds.Column{
		{Header: "DATA", Count: []string{"data"}},
	}}
	populated := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "cm1"},
		"data":     map[string]any{"a": "1", "b": "2"},
	}}
	empty := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "cm2"},
		"data":     map[string]any{},
	}}
	absent := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "cm3"},
	}}
	slice := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "sa1"},
		"data":     []any{"x", "y", "z"},
	}}
	tab := newTable(k, []unstructured.Unstructured{populated, empty, absent, slice}, false)
	rows := tab.Rows()
	require.Len(t, rows, 4)
	assert.Equal(t, []string{"cm1", "2", ""}, rows[0])
	assert.Equal(t, []string{"cm2", "0", ""}, rows[1])
	assert.Equal(t, []string{"cm3", "", ""}, rows[2])
	assert.Equal(t, []string{"sa1", "3", ""}, rows[3])
}

// TestTableRowsRenderAge asserts the AGE built-in: it is the LAST column on
// every kind table, formatted with duration.HumanDuration since the now()
// seam, and blank for an absent or unparseable creationTimestamp - never a
// fabricated "0s".
func TestTableRowsRenderAge(t *testing.T) {
	restore := now
	now = func() time.Time { return time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC) }
	defer func() { now = restore }()

	k := kinds.Kind{Name: "widget", Columns: []kinds.Column{
		{Header: "DISPLAY", Path: []string{"spec", "displayName"}},
	}}
	aged := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "w1", "creationTimestamp": "2026-01-01T00:00:00Z"},
		"spec":     map[string]any{"displayName": "Widget One"},
	}}
	absent := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "w2"},
		"spec":     map[string]any{"displayName": "Widget Two"},
	}}
	unparseable := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "w3", "creationTimestamp": "not-a-timestamp"},
		"spec":     map[string]any{"displayName": "Widget Three"},
	}}
	tab := newTable(k, []unstructured.Unstructured{aged, absent, unparseable}, false)
	assert.Equal(t, []string{"NAME", "DISPLAY", "AGE"}, tab.Columns())
	require.Len(t, tab.Rows(), 3)
	assert.Equal(t, []string{"w1", "Widget One", "120m"}, tab.Rows()[0])
	assert.Equal(t, []string{"w2", "Widget Two", ""}, tab.Rows()[1])
	assert.Equal(t, []string{"w3", "Widget Three", ""}, tab.Rows()[2])
}

// TestTableWideColumnsAndRowsOrder asserts the -o wide superset: WideColumns
// includes every column (Wide or not) in declaration order with AGE last -
// after the wide columns, not spliced in the middle - while the narrow
// Columns/Rows omit the Wide-marked columns entirely.
func TestTableWideColumnsAndRowsOrder(t *testing.T) {
	t.Parallel()
	k := kinds.Kind{Name: "widget", Columns: []kinds.Column{
		{Header: "DISPLAY", Path: []string{"spec", "displayName"}},
		{Header: "URL", Path: []string{"spec", "url"}, Wide: true},
	}}
	obj := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "w1"},
		"spec":     map[string]any{"displayName": "Widget One", "url": "https://example.com"},
	}}
	tab := newTable(k, []unstructured.Unstructured{obj}, false)

	assert.Equal(t, []string{"NAME", "DISPLAY", "AGE"}, tab.Columns())
	assert.Equal(t, [][]string{{"w1", "Widget One", ""}}, tab.Rows())

	assert.Equal(t, []string{"NAME", "DISPLAY", "URL", "AGE"}, tab.WideColumns())
	assert.Equal(t, [][]string{{"w1", "Widget One", "https://example.com", ""}}, tab.WideRows())
}

// TestShippedColumnRows spot-checks the column-audit rows shipped in this
// slice against real kinds.Lookup rows and wire-shaped fixtures, decoding
// fields per repo convention rather than string-matching rendered output.
func TestShippedColumnRows(t *testing.T) {
	t.Parallel()

	t.Run("project BACKING-NS default, DESCRIPTION wide", func(t *testing.T) {
		t.Parallel()
		k, ok := kinds.Lookup("project")
		require.True(t, ok)
		obj := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "p-1"},
			"spec":     map[string]any{"displayName": "Default", "clusterName": "c-x", "description": "team workloads"},
			"status":   map[string]any{"backingNamespace": "c-x-p-1"},
		}}
		tab := newTable(k, []unstructured.Unstructured{obj}, false)
		assert.Equal(t, []string{"p-1", "Default", "c-x", "c-x-p-1", ""}, tab.Rows()[0])
		assert.Equal(t, []string{"p-1", "Default", "c-x", "c-x-p-1", "team workloads", ""}, tab.WideRows()[0])
	})

	t.Run("node STATUS/VERSION default, OS-IMAGE wide", func(t *testing.T) {
		t.Parallel()
		k, ok := kinds.Lookup("node")
		require.True(t, ok)
		obj := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "n-1"},
			"status": map[string]any{
				"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
				"nodeInfo":   map[string]any{"kubeletVersion": "v1.30.5", "osImage": "K3s v1.30.5"},
			},
		}}
		tab := newTable(k, []unstructured.Unstructured{obj}, false)
		assert.Equal(t, []string{"n-1", "Ready", "v1.30.5", ""}, tab.Rows()[0])
		assert.Equal(t, []string{"n-1", "Ready", "v1.30.5", "K3s v1.30.5", ""}, tab.WideRows()[0])
	})

	t.Run("pod IP/NODE wide", func(t *testing.T) {
		t.Parallel()
		k, ok := kinds.Lookup("pod")
		require.True(t, ok)
		obj := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "pod-1"},
			"status":   map[string]any{"phase": "Running", "podIP": "10.0.0.1"},
			"spec":     map[string]any{"nodeName": "n-1"},
		}}
		tab := newTable(k, []unstructured.Unstructured{obj}, false)
		assert.Equal(t, []string{"pod-1", "Running", ""}, tab.Rows()[0])
		assert.Equal(t, []string{"pod-1", "Running", "10.0.0.1", "n-1", ""}, tab.WideRows()[0])
	})

	t.Run("service TYPE/CLUSTER-IP", func(t *testing.T) {
		t.Parallel()
		k, ok := kinds.Lookup("service")
		require.True(t, ok)
		obj := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "svc-1"},
			"spec":     map[string]any{"type": "ClusterIP", "clusterIP": "10.43.0.1"},
		}}
		tab := newTable(k, []unstructured.Unstructured{obj}, false)
		assert.Equal(t, []string{"svc-1", "ClusterIP", "10.43.0.1", ""}, tab.Rows()[0])
	})

	t.Run("configmap/secret DATA count", func(t *testing.T) {
		t.Parallel()
		cm, ok := kinds.Lookup("configmap")
		require.True(t, ok)
		cmObj := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "cm-1"},
			"data":     map[string]any{"a": "1", "b": "2"},
		}}
		assert.Equal(t, []string{"cm-1", "2", ""}, newTable(cm, []unstructured.Unstructured{cmObj}, false).Rows()[0])

		sec, ok := kinds.Lookup("secret")
		require.True(t, ok)
		secObj := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "sec-1"},
			"type":     "Opaque",
			"data":     map[string]any{"key": "v"},
		}}
		assert.Equal(t, []string{"sec-1", "Opaque", "1", ""}, newTable(sec, []unstructured.Unstructured{secObj}, false).Rows()[0])
	})

	t.Run("serviceaccount SECRETS count", func(t *testing.T) {
		t.Parallel()
		k, ok := kinds.Lookup("serviceaccount")
		require.True(t, ok)
		obj := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "sa-1"},
			"secrets":  []any{map[string]any{"name": "sa-1-token"}},
		}}
		tab := newTable(k, []unstructured.Unstructured{obj}, false)
		assert.Equal(t, []string{"sa-1", "1", ""}, tab.Rows()[0])
	})

	t.Run("fleet-cluster LAST-SEEN", func(t *testing.T) {
		t.Parallel()
		k, ok := kinds.Lookup("fleet-cluster")
		require.True(t, ok)
		obj := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "c-x"},
			"status":   map[string]any{"agent": map[string]any{"lastSeen": "2026-08-01T00:00:00Z"}},
		}}
		tab := newTable(k, []unstructured.Unstructured{obj}, false)
		assert.Equal(t, []string{"c-x", "2026-08-01T00:00:00Z", ""}, tab.Rows()[0])
	})

	t.Run("deployment UP-TO-DATE/AVAILABLE default 0 on omitted zero", func(t *testing.T) {
		t.Parallel()
		k, ok := kinds.Lookup("deployment")
		require.True(t, ok)
		// omitempty ints at zero are absent from the wire entirely.
		zero := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "d-zero"},
			"status":   map[string]any{},
		}}
		nonzero := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "d-1"},
			"status":   map[string]any{"updatedReplicas": int64(2), "availableReplicas": int64(1)},
		}}
		tab := newTable(k, []unstructured.Unstructured{zero, nonzero}, false)
		assert.Equal(t, []string{"d-zero", "0", "0", ""}, tab.Rows()[0])
		assert.Equal(t, []string{"d-1", "2", "1", ""}, tab.Rows()[1])
	})

	t.Run("daemonset DESIRED/CURRENT/AVAILABLE", func(t *testing.T) {
		t.Parallel()
		k, ok := kinds.Lookup("daemonset")
		require.True(t, ok)
		obj := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "ds-1"},
			"status": map[string]any{
				"desiredNumberScheduled": int64(3),
				"currentNumberScheduled": int64(3),
				// numberAvailable omitted: omitempty at zero.
			},
		}}
		tab := newTable(k, []unstructured.Unstructured{obj}, false)
		assert.Equal(t, []string{"ds-1", "3", "3", "0", ""}, tab.Rows()[0])
	})

	t.Run("replicaset DESIRED/CURRENT", func(t *testing.T) {
		t.Parallel()
		k, ok := kinds.Lookup("replicaset")
		require.True(t, ok)
		obj := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "rs-1"},
			"spec":     map[string]any{"replicas": int64(2)},
			"status":   map[string]any{"replicas": int64(2)},
		}}
		tab := newTable(k, []unstructured.Unstructured{obj}, false)
		assert.Equal(t, []string{"rs-1", "2", "2", ""}, tab.Rows()[0])
	})

	t.Run("provisioning-cluster VERSION/CLUSTER-NAME/READY", func(t *testing.T) {
		t.Parallel()
		k, ok := kinds.Lookup("provisioning-cluster")
		require.True(t, ok)
		obj := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "c-x"},
			"spec":     map[string]any{"kubernetesVersion": "v1.30.5+rke2r1"},
			"status":   map[string]any{"clusterName": "c-m-abc", "ready": "true"},
		}}
		tab := newTable(k, []unstructured.Unstructured{obj}, false)
		assert.Equal(t, []string{"c-x", "v1.30.5+rke2r1", "c-m-abc", "true", ""}, tab.Rows()[0])
	})

	t.Run("nodedriver URL wide", func(t *testing.T) {
		t.Parallel()
		k, ok := kinds.Lookup("nodedriver")
		require.True(t, ok)
		obj := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "amazonec2"},
			"spec":     map[string]any{"displayName": "Amazon EC2", "active": true, "builtin": true, "url": "local://"},
		}}
		tab := newTable(k, []unstructured.Unstructured{obj}, false)
		assert.Equal(t, []string{"amazonec2", "Amazon EC2", "true", "true", ""}, tab.Rows()[0])
		assert.Equal(t, []string{"amazonec2", "Amazon EC2", "true", "true", "local://", ""}, tab.WideRows()[0])
	})

	t.Run("kubeconfig DESCRIPTION wide", func(t *testing.T) {
		t.Parallel()
		k, ok := kinds.Lookup("kubeconfig")
		require.True(t, ok)
		obj := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "kc-1"},
			"status":   map[string]any{"summary": "1 cluster"},
			"spec":     map[string]any{"description": "ci pipeline"},
		}}
		tab := newTable(k, []unstructured.Unstructured{obj}, false)
		assert.Equal(t, []string{"kc-1", "1 cluster", ""}, tab.Rows()[0])
		assert.Equal(t, []string{"kc-1", "1 cluster", "ci pipeline", ""}, tab.WideRows()[0])
	})
}
