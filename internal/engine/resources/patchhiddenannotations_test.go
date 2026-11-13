package resources

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
)

// TestPatchDiffHidesTheHiddenAnnotationsOfEveryKind asserts, for every kind
// that has a patch command, that the preview diff of a patch shows the
// last-applied and objectset applied annotations as <redacted> exactly when
// the kind hides anything, in three forms: a patch that removes the whole
// annotations map, one that adds it, and one that changes only the objectset
// applied annotation. The owner annotation stays visible in every form.
//
// managementtoken and clusterregistrationtoken run here through the generic
// patch although the command registry replaces theirs with a refusal.
func TestPatchDiffHidesTheHiddenAnnotationsOfEveryKind(t *testing.T) {
	t.Parallel()
	const (
		lastApplied = "kubectl.kubernetes.io/last-applied-configuration"
		applied     = "objectset.rio.cattle.io/applied"
	)
	annotations := map[string]any{lastApplied: "old-last-applied", applied: "old-applied", "owner": "team-a"}
	shown := func(hides bool, m map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			if hides && (k == lastApplied || k == applied) {
				v = engine.Redacted
			}
			out[k] = v
		}
		return out
	}
	object := func(t *testing.T, k kinds.Kind, withAnnotations bool) string {
		t.Helper()
		apiVersion := k.GVR.Version
		if k.GVR.Group != "" {
			apiVersion = k.GVR.Group + "/" + k.GVR.Version
		}
		meta := map[string]any{"name": "obj", "namespace": "default", "resourceVersion": "5"}
		if withAnnotations {
			meta["annotations"] = annotations
		}
		b, err := json.Marshal(map[string]any{"apiVersion": apiVersion, "kind": k.K8sKind, "metadata": meta})
		require.NoError(t, err)
		return string(b)
	}
	mergePatch := func(t *testing.T, value any) string {
		t.Helper()
		b, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": value}})
		require.NoError(t, err)
		return string(b)
	}

	var skippedKeys []string
	for _, k := range kinds.All() {
		if k.NoPatchReason != "" {
			skippedKeys = append(skippedKeys, k.Key)
			continue
		}
		hides := k.Redaction().Hides()

		eff, _, err := planTyped(t, k.Key, "obj", object(t, k, true), mergePatch(t, nil), "merge")
		require.NoError(t, err, k.Key)
		d, ok := diffAt(eff, "/metadata/annotations")
		require.True(t, ok, "%s diff: %+v", k.Key, eff.Diff)
		assert.Equal(t, shown(hides, annotations), d.From, "%s removes the map", k.Key)
		assert.Nil(t, d.To, k.Key)

		eff, _, err = planTyped(t, k.Key, "obj", object(t, k, false), mergePatch(t, annotations), "merge")
		require.NoError(t, err, k.Key)
		d, ok = diffAt(eff, "/metadata/annotations")
		require.True(t, ok, "%s diff: %+v", k.Key, eff.Diff)
		assert.Nil(t, d.From, k.Key)
		assert.Equal(t, shown(hides, annotations), d.To, "%s adds the map", k.Key)

		eff, _, err = planTyped(t, k.Key, "obj", object(t, k, true), mergePatch(t, map[string]any{applied: "new-applied"}), "merge")
		require.NoError(t, err, k.Key)
		d, ok = diffAt(eff, "/metadata/annotations/objectset.rio.cattle.io~1applied")
		require.True(t, ok, "%s diff: %+v", k.Key, eff.Diff)
		assert.Equal(t, engine.PointerFor([]string{"metadata", "annotations", applied}), d.Path, k.Key)
		if hides {
			assert.Equal(t, engine.Redacted, d.From, "%s changes the value", k.Key)
			assert.Equal(t, engine.Redacted, d.To, "%s changes the value", k.Key)
		} else {
			assert.Equal(t, "old-applied", d.From, "%s changes the value", k.Key)
			assert.Equal(t, "new-applied", d.To, "%s changes the value", k.Key)
		}
	}
	// These four have a refusal instead of a generated patch. A row that gets
	// a NoPatchReason fails here until it is named on purpose.
	assert.ElementsMatch(t, []string{"etcdsnapshot", "clusterrepo", "app", "operation"}, skippedKeys)
}
