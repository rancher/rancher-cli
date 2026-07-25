package fidelity

import (
	"testing"

	"github.com/stretchr/testify/assert"
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
		"status": map[string]any{"bearerToken": "ext/name:secret"},
	}
	Normalize(body, DefaultNormalizeRules())
	meta := body["metadata"].(map[string]any)
	assert.Equal(t, "<uid>", meta["uid"])
	assert.Equal(t, "<rv>", meta["resourceVersion"])
	assert.NotContains(t, meta, "managedFields")
	assert.Equal(t, "e2e-fid-x", meta["name"], "untargeted fields stay")
	assert.Equal(t, "<secret>", body["status"].(map[string]any)["bearerToken"])
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
