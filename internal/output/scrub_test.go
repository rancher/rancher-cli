package output

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScrubManagedFieldsDeletesOnlyUnderMetadata(t *testing.T) {
	t.Parallel()
	tree := map[string]any{
		"metadata": map[string]any{
			"name":          "cm",
			"managedFields": []any{map[string]any{"manager": "k3s"}},
		},
		"managedFields": "user-data-survives",
		"nested": []any{
			map[string]any{
				"metadata": map[string]any{"managedFields": "x", "uid": "u1"},
			},
		},
	}
	scrubManagedFields(tree)
	meta := tree["metadata"].(map[string]any)
	assert.NotContains(t, meta, "managedFields")
	assert.Equal(t, "cm", meta["name"])
	assert.Equal(t, "user-data-survives", tree["managedFields"],
		"a managedFields key outside a metadata map survives (MF-2)")
	inner := tree["nested"].([]any)[0].(map[string]any)["metadata"].(map[string]any)
	assert.NotContains(t, inner, "managedFields")
	assert.Equal(t, "u1", inner["uid"])
}

func TestScrubbedEnvelopeTreeFastPath(t *testing.T) {
	t.Parallel()
	env := NewEnvelope("X", map[string]any{"metadata": map[string]any{"name": "clean"}})
	tree, hit, err := scrubbedEnvelopeTree(env)
	require.NoError(t, err)
	assert.False(t, hit, "no managedFields literal means no tree (MF-2 fast path)")
	assert.Nil(t, tree)
}

func TestScrubbedEnvelopeTreeSlowPath(t *testing.T) {
	t.Parallel()
	env := NewEnvelope("X", map[string]any{
		"metadata": map[string]any{
			"name":          "cm",
			"managedFields": []any{map[string]any{"manager": "k3s"}},
			"generation":    int64(9007199254740993),
		},
	})
	tree, hit, err := scrubbedEnvelopeTree(env)
	require.NoError(t, err)
	require.True(t, hit)
	data := tree.(map[string]any)["data"].(map[string]any)
	meta := data["metadata"].(map[string]any)
	assert.NotContains(t, meta, "managedFields")
	assert.Equal(t, "9007199254740993", meta["generation"].(interface{ String() string }).String(),
		"large ints survive as json.Number, no float64 round-trip (MF-2)")
}
