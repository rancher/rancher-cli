package fidelity

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeScrubsAndDeletes(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"metadata": map[string]any{
			"name":            "e2e-fid-x",
			"uid":             "0e3a…",
			"resourceVersion": "4711",
			"managedFields":   []any{map[string]any{"manager": "kubectl"}},
		},
		"status": map[string]any{
			"bearerToken":    "ext/name:secret",
			"value":          "raw-secret",
			"hash":           "$3:1:salt:digest",
			"lastUpdateTime": "2026-07-25T02:00:00Z",
		},
	}
	Normalize(body, DefaultNormalizeRules())
	meta := body["metadata"].(map[string]any)
	assert.Equal(t, "<uid>", meta["uid"])
	assert.Equal(t, "<rv>", meta["resourceVersion"])
	assert.NotContains(t, meta, "managedFields")
	assert.Equal(t, "e2e-fid-x", meta["name"], "untargeted fields stay")
	status := body["status"].(map[string]any)
	assert.Equal(t, "<secret>", status["bearerToken"])
	assert.Equal(t, "<secret>", status["value"], "the bare token value is a live secret too")
	assert.Equal(t, "<secret>", status["hash"], "the stored token digest is scrubbed too")
	assert.Equal(t, "<ts>", status["lastUpdateTime"], "the per-create timestamp is volatile")
}

func TestNormalizeTraversesArraysAndWildcards(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"data": []any{
			map[string]any{"metadata": map[string]any{"resourceVersion": "1"}},
			map[string]any{"metadata": map[string]any{"resourceVersion": "2"}},
		},
	}
	Normalize(body, []NormalizeRule{{Path: "data.metadata.resourceVersion", Replace: "<rv>"}})
	for _, el := range body["data"].([]any) {
		meta := el.(map[string]any)["metadata"].(map[string]any)
		assert.Equal(t, "<rv>", meta["resourceVersion"])
	}

	wild := map[string]any{"a": map[string]any{"ts": "x"}, "b": map[string]any{"ts": "y"}}
	Normalize(wild, []NormalizeRule{{Path: "*.ts", Replace: "<ts>"}})
	assert.Equal(t, "<ts>", wild["a"].(map[string]any)["ts"])
	assert.Equal(t, "<ts>", wild["b"].(map[string]any)["ts"])
}

func TestNormalizeMissingPathIsNoOp(t *testing.T) {
	t.Parallel()
	body := map[string]any{"kind": "Secret"}
	Normalize(body, DefaultNormalizeRules())
	assert.Equal(t, map[string]any{"kind": "Secret"}, body)
}

func TestKeepPrefixKeepsMatchingScrubsOthers(t *testing.T) {
	t.Parallel()
	out, err := keepPrefix([]any{"system:masters", "genericsaml_group://admins", "system:authenticated"}, "system:")
	require.NoError(t, err)
	assert.Equal(t, []any{"system:masters", "<group>", "system:authenticated"}, out, "order and count are kept")
}

func TestKeepPrefixAllKept(t *testing.T) {
	t.Parallel()
	out, err := keepPrefix([]any{"system:a", "system:b"}, "system:")
	require.NoError(t, err)
	assert.Equal(t, []any{"system:a", "system:b"}, out)
}

func TestKeepPrefixAllReplaced(t *testing.T) {
	t.Parallel()
	out, err := keepPrefix([]any{"azuread_user://x", "okta_user://y"}, "system:")
	require.NoError(t, err)
	assert.Equal(t, []any{"<group>", "<group>"}, out)
}

func TestKeepPrefixNonArrayIsAnError(t *testing.T) {
	t.Parallel()
	_, err := keepPrefix("not-an-array", "system:")
	require.Error(t, err)
}

// TestKeepPrefixRuleNeverLeavesANonArrayInPlace asserts a KeepPrefix rule
// addressing a value that is not an array of strings replaces it whole:
// the rule named the value because it carries instance data.
func TestKeepPrefixRuleNeverLeavesANonArrayInPlace(t *testing.T) {
	t.Parallel()
	body := map[string]any{"status": map[string]any{"userInfo": map[string]any{"groups": "admins@example.com"}}}
	Normalize(body, []NormalizeRule{{Path: "status.userInfo.groups", KeepPrefix: "system:"}})
	assert.Equal(t, "<group>", body["status"].(map[string]any)["userInfo"].(map[string]any)["groups"])
}

func TestNormalizeKeepPrefixAppliesThroughAPath(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"status": map[string]any{
			"userInfo": map[string]any{
				"groups": []any{"system:masters", "genericsaml_group://admins", "system:authenticated"},
			},
		},
	}
	Normalize(body, []NormalizeRule{{Path: "status.userInfo.groups", KeepPrefix: "system:"}})
	groups := body["status"].(map[string]any)["userInfo"].(map[string]any)["groups"]
	assert.Equal(t, []any{"system:masters", "<group>", "system:authenticated"}, groups)
}

func TestNormalizeTerminalWildcardScrubsEveryKey(t *testing.T) {
	t.Parallel()
	body := map[string]any{
		"metadata": map[string]any{
			"labels": map[string]any{
				"cattle.io/user-id":               "u-abc",
				"authn.management.cattle.io/kind": "",
				"other":                           "keep-shape-only",
			},
		},
	}
	Normalize(body, []NormalizeRule{{Path: "metadata.labels.*", Replace: "<label>"}})
	labels := body["metadata"].(map[string]any)["labels"].(map[string]any)
	require.Len(t, labels, 3, "the wildcard replaces values, never keys")
	for k, v := range labels {
		assert.Equal(t, "<label>", v, "every label value is scrubbed, including the dotted key %q", k)
	}
}

// TestNormalizeSortByOrdersAnArrayByAField asserts SortBy orders the
// addressed array by the named string field, stably, before any Replace
// rule scrubs the names the server ordered by.
func TestNormalizeSortByOrdersAnArrayByAField(t *testing.T) {
	t.Parallel()
	body := map[string]any{"items": []any{
		map[string]any{"metadata": map[string]any{"name": "rb-zzz"}, "roleRef": map[string]any{"name": "project-member"}},
		map[string]any{"metadata": map[string]any{"name": "rb-aaa"}, "roleRef": map[string]any{"name": "edit"}},
	}}
	Normalize(body, []NormalizeRule{{Path: "items", SortBy: "roleRef.name"}, {Path: "items.metadata.name", Replace: "<name>"}})
	items := body["items"].([]any)
	assert.Equal(t, "edit", items[0].(map[string]any)["roleRef"].(map[string]any)["name"])
	assert.Equal(t, "<name>", items[0].(map[string]any)["metadata"].(map[string]any)["name"])
}

// TestNormalizeSortByDotOrdersAnArrayOfStrings asserts SortBy "." orders
// an array of strings by value.
func TestNormalizeSortByDotOrdersAnArrayOfStrings(t *testing.T) {
	t.Parallel()
	body := map[string]any{"metadata": map[string]any{"finalizers": []any{"b.cattle.io", "a.cattle.io", "c.cattle.io"}}}
	Normalize(body, []NormalizeRule{{Path: "metadata.finalizers", SortBy: "."}})
	assert.Equal(t, []any{"a.cattle.io", "b.cattle.io", "c.cattle.io"}, body["metadata"].(map[string]any)["finalizers"])
}

// TestNormalizeDropPrefixFiltersAnArrayAndItsCount asserts DropPrefix
// removes the elements whose string at DropBy starts with the prefix,
// keeps the order of the rest, and sets CountKey to the number kept,
// deleting it when none is kept; an object with no CountKey key gains
// none.
func TestNormalizeDropPrefixFiltersAnArrayAndItsCount(t *testing.T) {
	t.Parallel()
	body := map[string]any{"count": float64(3), "data": []any{
		map[string]any{"id": "kube-system"}, map[string]any{"id": "e2e-fid-explain"}, map[string]any{"id": "default"},
	}}
	Normalize(body, []NormalizeRule{{Path: "data", DropBy: "id", DropPrefix: "e2e-fid-", CountKey: "count"}})
	assert.Equal(t, []any{map[string]any{"id": "kube-system"}, map[string]any{"id": "default"}}, body["data"])
	assert.Equal(t, float64(2), body["count"])

	body = map[string]any{"data": []any{"e2e-fid-a", "b"}}
	Normalize(body, []NormalizeRule{{Path: "data", DropBy: ".", DropPrefix: "e2e-fid-", CountKey: "count"}})
	assert.Equal(t, []any{"b"}, body["data"])
	assert.NotContains(t, body, "count")

	body = map[string]any{"count": float64(1), "data": []any{map[string]any{"id": "e2e-fid-explain"}}}
	Normalize(body, []NormalizeRule{{Path: "data", DropBy: "id", DropPrefix: "e2e-fid-", CountKey: "count"}})
	assert.Equal(t, []any{}, body["data"])
	assert.NotContains(t, body, "count", "steve omits count on an empty collection")
}
