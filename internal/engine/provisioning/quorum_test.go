package provisioning

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// clusterWithPools builds a minimal provisioning Cluster object carrying
// exactly the machinePools ComputeQuorumDelta reads.
func clusterWithPools(pools ...map[string]any) *unstructured.Unstructured {
	items := make([]any, len(pools))
	for i, p := range pools {
		items[i] = p
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "provisioning.cattle.io/v1",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": "c1", "namespace": "fleet-default"},
		"spec": map[string]any{
			"rkeConfig": map[string]any{
				"machinePools": items,
			},
		},
	}}
}

// pool builds one machinePools entry. A nil quantity omits the field
// entirely, exercising the documented default-to-1 behavior.
func pool(etcd bool, quantity any) map[string]any {
	p := map[string]any{"etcdRole": etcd}
	if quantity != nil {
		p["quantity"] = quantity
	}
	return p
}

func TestComputeQuorumDelta(t *testing.T) {
	t.Parallel()

	t.Run("3 to 5 grows: no crossing, no even-count note (5 is odd)", func(t *testing.T) {
		t.Parallel()
		orig := clusterWithPools(pool(true, int64(3)))
		patched := clusterWithPools(pool(true, int64(5)))

		delta, ok := ComputeQuorumDelta(orig, patched)
		require.True(t, ok)
		assert.Equal(t, 3, delta.Before)
		assert.Equal(t, 5, delta.After)
		assert.False(t, delta.Crossing)
		assert.NotContains(t, delta.Note, "even etcd counts")
		assert.NotContains(t, delta.Note, "quorum majority")
		assert.Contains(t, delta.Note, "3 to 5")
	})

	t.Run("3 to 2 shrinks: crossing", func(t *testing.T) {
		t.Parallel()
		orig := clusterWithPools(pool(true, int64(3)))
		patched := clusterWithPools(pool(true, int64(2)))

		delta, ok := ComputeQuorumDelta(orig, patched)
		require.True(t, ok)
		assert.Equal(t, 3, delta.Before)
		assert.Equal(t, 2, delta.After)
		assert.True(t, delta.Crossing)
		assert.Contains(t, delta.Note, "quorum majority")
	})

	t.Run("3 to 4 grows to an even count: even-count note, no crossing", func(t *testing.T) {
		t.Parallel()
		orig := clusterWithPools(pool(true, int64(3)))
		patched := clusterWithPools(pool(true, int64(4)))

		delta, ok := ComputeQuorumDelta(orig, patched)
		require.True(t, ok)
		assert.Equal(t, 3, delta.Before)
		assert.Equal(t, 4, delta.After)
		assert.False(t, delta.Crossing)
		assert.Contains(t, delta.Note, "even etcd counts")
		assert.NotContains(t, delta.Note, "quorum majority")
	})

	t.Run("etcd role flip true to false counts as removal of that pool's quantity", func(t *testing.T) {
		t.Parallel()
		orig := clusterWithPools(pool(true, int64(2)), pool(true, int64(1)))
		patched := clusterWithPools(pool(false, int64(2)), pool(true, int64(1)))

		delta, ok := ComputeQuorumDelta(orig, patched)
		require.True(t, ok)
		assert.Equal(t, 3, delta.Before)
		assert.Equal(t, 1, delta.After)
	})

	t.Run("a non-etcd-affecting patch reports no change", func(t *testing.T) {
		t.Parallel()
		orig := clusterWithPools(pool(true, int64(3)), pool(false, int64(2)))
		patched := clusterWithPools(pool(true, int64(3)), pool(false, int64(5)))

		delta, ok := ComputeQuorumDelta(orig, patched)
		assert.False(t, ok)
		assert.Equal(t, QuorumDelta{}, delta)
	})

	t.Run("a missing quantity defaults to 1", func(t *testing.T) {
		t.Parallel()
		orig := clusterWithPools(pool(true, nil))
		patched := clusterWithPools(pool(true, int64(2)))

		delta, ok := ComputeQuorumDelta(orig, patched)
		require.True(t, ok)
		assert.Equal(t, 1, delta.Before)
		assert.Equal(t, 2, delta.After)
	})
}
