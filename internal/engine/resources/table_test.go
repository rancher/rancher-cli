package resources

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

// registeredKind returns the registered row with key. A table hides values
// only for a registered row, so these tests never build a literal kind.
func registeredKind(t *testing.T, key string) kinds.Kind {
	t.Helper()
	k, ok := kinds.ByKey(key)
	require.True(t, ok, key)
	return k
}

// marshalTableObject marshals a table of one object and decodes the result.
func marshalTableObject(t *testing.T, k kinds.Kind, obj map[string]any) map[string]any {
	t.Helper()
	b, err := newTable(k, []unstructured.Unstructured{{Object: obj}}, true).MarshalJSON()
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(b, &decoded))
	return decoded
}

// TestTableMarshalJSONRedactsKindPaths verifies that a kind's hidden paths
// are absent from -o json output, also for a path nested below another
// field. The test decodes the marshaled JSON rather than string-matching, so
// whitespace or key ordering can never cause a false pass.
func TestTableMarshalJSONRedactsKindPaths(t *testing.T) {
	t.Parallel()
	decoded := marshalTableObject(t, registeredKind(t, "token"), map[string]any{
		"metadata": map[string]any{"name": "t1"},
		"spec":     map[string]any{"description": "visible"},
		"status":   map[string]any{"bearerToken": "must-be-gone", "value": "also-gone", "expiresAt": "keep"},
	})
	spec, _ := decoded["spec"].(map[string]any)
	assert.Equal(t, "visible", spec["description"], "non-redacted field must be present")
	status, _ := decoded["status"].(map[string]any)
	require.NotNil(t, status, "parent of a nested redact path must still be present")
	assert.NotContains(t, status, "bearerToken", "nested redact field must be absent")
	assert.NotContains(t, status, "value", "nested redact field must be absent")
	assert.Equal(t, "keep", status["expiresAt"], "sibling of redacted nested field must be present")

	decoded = marshalTableObject(t, registeredKind(t, "user"), map[string]any{
		"metadata": map[string]any{"name": "u-1"},
		"username": "alice",
		"password": "must-be-gone",
	})
	assert.Equal(t, "alice", decoded["username"])
	assert.NotContains(t, decoded, "password", "RedactPaths root field must be absent")
}

// TestTableMarshalJSONRedactsListItems verifies that redaction applies to
// every item in a list result, not just the first.
func TestTableMarshalJSONRedactsListItems(t *testing.T) {
	t.Parallel()
	items := []unstructured.Unstructured{
		{Object: map[string]any{"metadata": map[string]any{"name": "a"}, "token": "cred-a"}},
		{Object: map[string]any{"metadata": map[string]any{"name": "b"}, "token": "cred-b"}},
	}
	tab := newTable(registeredKind(t, "managementtoken"), items, false)
	b, err := tab.MarshalJSON()
	require.NoError(t, err)

	var decoded []map[string]any
	require.NoError(t, json.Unmarshal(b, &decoded))
	require.Len(t, decoded, 2)
	assert.NotContains(t, decoded[0], "token", "token must be absent from item 0")
	assert.NotContains(t, decoded[1], "token", "token must be absent from item 1")
	assert.Equal(t, "a", decoded[0]["metadata"].(map[string]any)["name"])
	assert.Equal(t, "b", decoded[1]["metadata"].(map[string]any)["name"])
}

// TestTableMarshalJSONHidesWhatThePreviewHides verifies the table hides a
// path in any letter case, a machine config Secret field in any letter
// case, the last-applied annotation of a row that hides anything, and
// nothing of a row that hides nothing.
func TestTableMarshalJSONHidesWhatThePreviewHides(t *testing.T) {
	t.Parallel()
	annotations := func() map[string]any {
		return map[string]any{kinds.LastAppliedAnnotation: `{"x":"s3cretA"}`, "example.invalid/outside": "outsideA"}
	}
	outside := map[string]any{"example.invalid/outside": "outsideA"}

	decoded := marshalTableObject(t, registeredKind(t, "user"), map[string]any{
		"metadata": map[string]any{"name": "u-1"}, "Password": "s3cretA", "username": "alice",
	})
	assert.NotContains(t, decoded, "Password", "a path written in another letter case is hidden")
	assert.Equal(t, "alice", decoded["username"])

	decoded = marshalTableObject(t, registeredKind(t, "amazonec2config"), map[string]any{
		"metadata":       map[string]any{"name": "w", "annotations": annotations()},
		"SSHKeyContents": "s3cretA", "sessiontoken": "s3cretB", "region": "us-east-1",
	})
	assert.NotContains(t, decoded, "SSHKeyContents", "a machine config Secret field in another letter case is hidden")
	assert.NotContains(t, decoded, "sessiontoken")
	assert.Equal(t, "us-east-1", decoded["region"])
	assert.Equal(t, map[string]any{"name": "w", "annotations": outside}, decoded["metadata"], "the last-applied annotation is hidden")

	decoded = marshalTableObject(t, registeredKind(t, "namespace"), map[string]any{
		"metadata": map[string]any{"name": "ns", "annotations": annotations()},
	})
	assert.Equal(t, map[string]any{"name": "ns", "annotations": map[string]any{
		kinds.LastAppliedAnnotation: `{"x":"s3cretA"}`, "example.invalid/outside": "outsideA",
	}}, decoded["metadata"], "a row that hides nothing shows the annotation")
}

// TestTableMarshalJSONKeepsASecretPayload verifies a get of a secret shows
// data and stringData in any letter case and hides the last-applied
// annotation, for one object and for each item of a list; any other
// Sensitive row hides its payload.
func TestTableMarshalJSONKeepsASecretPayload(t *testing.T) {
	t.Parallel()
	obj := func() map[string]any {
		return map[string]any{
			"metadata": map[string]any{"name": "s", "annotations": map[string]any{
				kinds.LastAppliedAnnotation: `{"x":"s3cretA"}`, "example.invalid/outside": "outsideA",
			}},
			"data": map[string]any{"k": "czNjcmV0QQ=="}, "StringData": map[string]any{"k": "s3cretA"},
		}
	}
	want := map[string]any{
		"metadata":   map[string]any{"name": "s", "annotations": map[string]any{"example.invalid/outside": "outsideA"}},
		"data":       map[string]any{"k": "czNjcmV0QQ=="},
		"StringData": map[string]any{"k": "s3cretA"},
	}
	secret := registeredKind(t, "secret")
	assert.Equal(t, want, marshalTableObject(t, secret, obj()))

	b, err := newTable(secret, []unstructured.Unstructured{{Object: obj()}, {Object: obj()}}, false).MarshalJSON()
	require.NoError(t, err)
	var list []map[string]any
	require.NoError(t, json.Unmarshal(b, &list))
	assert.Equal(t, []map[string]any{want, want}, list)

	decoded := marshalTableObject(t, registeredKind(t, "setting"), obj())
	assert.NotContains(t, decoded, "data")
	assert.NotContains(t, decoded, "StringData")
}

// TestTableMarshalJSONDoesNotMutateOriginal verifies that marshaling a table
// leaves the original unstructured object as it was, for a row that removes
// a value and for one that removes nothing (important: a command may hold
// the object in memory after serialization).
func TestTableMarshalJSONDoesNotMutateOriginal(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"user", "secret", "namespace"} {
		obj := unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "w1", "annotations": map[string]any{
				kinds.LastAppliedAnnotation: "must-survive-in-original",
			}},
			"password": "must-survive-in-original",
			"data":     map[string]any{"k": "must-survive-in-original"},
		}}
		want := obj.DeepCopy().Object
		for _, single := range []bool{true, false} {
			_, err := newTable(registeredKind(t, key), []unstructured.Unstructured{obj}, single).MarshalJSON()
			require.NoError(t, err)
			assert.Equal(t, want, obj.Object, "the original unstructured object must not be mutated: %s", key)
		}
	}
}

// TestRedactDiffBlanksKindPathEntries asserts that engine.RedactDiff replaces
// From/To with "<redacted>" for diff entries at or under a RedactPaths
// path, and leaves unrelated entries intact.
func TestRedactDiffBlanksKindPathEntries(t *testing.T) {
	t.Parallel()
	diff := []safety.DiffEntry{
		{Path: "/token", From: "old-cred", To: "new-cred"},
		{Path: "/token/sub", From: "sub-old", To: "sub-new"},
		{Path: "/description", From: "old-desc", To: "new-desc"},
		{Path: "/tokenExtra", From: "keep-old", To: "keep-new"}, // prefix match must not fire
	}
	diff = engine.RedactDiff(diff, kinds.RowlessRedaction(schema.GroupVersionResource{}, "a test resource with no kind row", []string{"token"}))

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
// fields are always emitted (never omitempty).
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
// spec.value/status.default fallback). lockedValue is an omitempty *bool -
// absent (unlocked) must render blank, never "false", since a locked-false
// feature is a real, different state. VALUE is asserted both directions:
// spec.value absent falls through to status.default, and spec.value present
// (even false) wins over status.default.
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

// TestShippedColumnRows spot-checks the column rows against real kinds.Lookup
// rows and wire-shaped fixtures, decoding fields per repo convention rather
// than string-matching rendered output.
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
