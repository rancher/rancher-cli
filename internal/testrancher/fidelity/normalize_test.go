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
