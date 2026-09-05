package engine

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiffObjectsSkipsManaged(t *testing.T) {
	t.Parallel()
	a := map[string]any{
		"metadata": map[string]any{
			"name":            "foo",
			"resourceVersion": "1",
			"managedFields":   []any{map[string]any{"manager": "kubectl"}},
		},
		"spec": map[string]any{"replicas": float64(1)},
	}
	b := map[string]any{
		"metadata": map[string]any{
			"name":            "foo",
			"resourceVersion": "2",
			"managedFields":   []any{map[string]any{"manager": "kubectl"}},
		},
		"spec": map[string]any{"replicas": float64(2)},
	}
	diffs := DiffObjects(a, b)
	require.Len(t, diffs, 1)
	assert.Equal(t, "/spec/replicas", diffs[0].Path)
}

func TestDiffObjectsEscapesSlashAndTildeKeys(t *testing.T) {
	t.Parallel()
	a := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				"app.kubernetes.io/name": "old",
				"a~b":                    "old",
			},
		},
	}
	b := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				"app.kubernetes.io/name": "new",
				"a~b":                    "new",
			},
		},
	}
	diffs := DiffObjects(a, b)
	assert.Equal(t, []safety.DiffEntry{
		{Path: "/metadata/annotations/app.kubernetes.io~1name", From: "old", To: "new"},
		{Path: "/metadata/annotations/a~0b", From: "old", To: "new"},
	}, diffs)
}

// TestDiffObjectsTypeMismatchAtMetaDoesNotLeakManagedFields is the latent bug
// repro: when a type mismatch forces /metadata to be emitted wholesale as
// DiffEntry.From/To, the emitted copies must not carry managedFields or
// resourceVersion.
func TestDiffObjectsTypeMismatchAtMetaDoesNotLeakManagedFields(t *testing.T) {
	t.Parallel()
	// a has /metadata as a map (the normal case); b has /metadata as a string
	// (a synthetic type mismatch). Without the fix, the DiffEntry.From copy
	// contains managedFields because the skip check runs on the full path
	// (/metadata), which doesn't match the skip list (/metadata/managedFields).
	a := map[string]any{
		"metadata": map[string]any{
			"name":            "foo",
			"resourceVersion": "1",
			"managedFields":   []any{map[string]any{"manager": "kubectl"}},
		},
	}
	b := map[string]any{
		"metadata": "not-a-map", // forces type mismatch on the map branch
	}
	diffs := DiffObjects(a, b)
	require.Len(t, diffs, 1)
	assert.Equal(t, "/metadata", diffs[0].Path)

	from, ok := diffs[0].From.(map[string]any)
	require.True(t, ok, "From must still be a map (just stripped)")
	assert.NotContains(t, from, "managedFields", "managedFields must not leak into the diff entry")
	assert.NotContains(t, from, "resourceVersion", "resourceVersion must not leak into the diff entry")
	assert.Equal(t, "foo", from["name"], "non-skipped keys must be preserved")
}

// The leak paths the branch review reproduced: the strip must apply to
// every wholesale-emitted value (either side, any branch) and recurse into
// nested maps, not just direct children of the mismatch prefix. The review's
// third repro, "not-a-map"-vs-object, drives the identical default-branch
// scrub the nil-vs-object case assertions (nil and a string both fall through the
// type switch), so it is not duplicated here.
func TestDiffObjectsScrubsAllWholesaleEmissions(t *testing.T) {
	t.Parallel()
	full := map[string]any{
		"metadata": map[string]any{
			"name":            "foo",
			"resourceVersion": "1",
			"managedFields":   []any{map[string]any{"manager": "kubectl"}},
		},
		"spec": map[string]any{"replicas": float64(1)},
	}

	t.Run("nil-vs-object emits a scrubbed To", func(t *testing.T) {
		t.Parallel()
		diffs := DiffObjects(nil, full)
		require.Len(t, diffs, 1)
		to, ok := diffs[0].To.(map[string]any)
		require.True(t, ok)
		meta, ok := to["metadata"].(map[string]any)
		require.True(t, ok)
		assert.NotContains(t, meta, "managedFields")
		assert.NotContains(t, meta, "resourceVersion")
	})

	t.Run("object-vs-nil emits a scrubbed From recursively", func(t *testing.T) {
		t.Parallel()
		diffs := DiffObjects(full, nil)
		require.Len(t, diffs, 1)
		from, ok := diffs[0].From.(map[string]any)
		require.True(t, ok)
		meta, ok := from["metadata"].(map[string]any)
		require.True(t, ok)
		assert.NotContains(t, meta, "managedFields")
		assert.NotContains(t, meta, "resourceVersion")
		assert.Equal(t, "foo", meta["name"])
		// Sibling subtrees that cannot contain a skipped path are preserved.
		assert.Equal(t, map[string]any{"replicas": float64(1)}, from["spec"])
	})
}
