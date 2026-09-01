package provisioning

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// familyClient fakes the Get/Update pair every cluster-family verb
// exercises: Get serves a fixed object (or getErr), Update records every
// call — dry-run and real — for assertion.
type familyClient struct {
	obj    *unstructured.Unstructured
	getErr error

	updated       *unstructured.Unstructured
	updatedNS     string
	updateDryRuns []bool
}

func (f *familyClient) Get(_ context.Context, _ schema.GroupVersionResource, _, _ string) (*unstructured.Unstructured, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.obj, nil
}
func (f *familyClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	return &unstructured.UnstructuredList{}, nil
}
func (f *familyClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (f *familyClient) Update(_ context.Context, _ schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	f.updated, f.updatedNS = obj, ns
	f.updateDryRuns = append(f.updateDryRuns, dryRun)
	return obj, nil
}
func (f *familyClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}
func (f *familyClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}

var _ engine.ClusterClient = (*familyClient)(nil)

func planFamily(t *testing.T, cmd runtime.Command, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "cluster-family commands must be MutatingCommand")
	return mc.Plan(t.Context(), in)
}

// familyPool builds a machinePools entry. min/max are nil for an unmanaged
// pool.
func familyPool(name string, quantity int64, etcd, cp, worker bool, min, max *int64) map[string]any {
	p := map[string]any{
		"name": name, "quantity": quantity,
		"etcdRole": etcd, "controlPlaneRole": cp, "workerRole": worker,
	}
	if min != nil {
		p["autoscalingMinSize"] = *min
	}
	if max != nil {
		p["autoscalingMaxSize"] = *max
	}
	return p
}

// familyClusterObj builds a provisioning Cluster fixture. A nil pools slice
// produces a pool-less cluster (spec.rkeConfig present but no
// machinePools field, the imported/custom shape); an empty non-nil slice is
// never used by these tests (createcluster.go never emits it either).
func familyClusterObj(name, rv string, pools []any, paused bool) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
	}}
	if pools != nil {
		_ = unstructured.SetNestedSlice(obj.Object, pools, "spec", "rkeConfig", "machinePools")
	} else {
		_ = unstructured.SetNestedMap(obj.Object, map[string]any{}, "spec", "rkeConfig")
	}
	if paused {
		_ = unstructured.SetNestedField(obj.Object, "true", "metadata", "annotations", clusterAutoscalerPausedAnnotation)
	}
	obj.SetResourceVersion(rv)
	return obj
}

func TestClusterMachinePools(t *testing.T) {
	t.Run("pool-less cluster (no rkeConfig.machinePools) reports found=false", func(t *testing.T) {
		obj := familyClusterObj("imported-1", "1", nil, false)
		pools, found := clusterMachinePools(obj)
		assert.False(t, found)
		assert.Empty(t, pools)
	})

	t.Run("present but empty machinePools reports found=true", func(t *testing.T) {
		obj := familyClusterObj("mid-transition", "1", []any{}, false)
		pools, found := clusterMachinePools(obj)
		assert.True(t, found, "the field exists — a different shape from wholly absent")
		assert.Empty(t, pools)
	})

	t.Run("populated cluster reports found=true with a fresh copy", func(t *testing.T) {
		obj := familyClusterObj("node-1", "1", []any{familyPool("pool1", 3, false, false, true, nil, nil)}, false)
		pools, found := clusterMachinePools(obj)
		require.True(t, found)
		require.Len(t, pools, 1)

		// Mutating the returned slice must not affect a second extraction —
		// NestedSlice always deep-copies (findMachinePool/scale rely on this).
		pools[0].(map[string]any)["quantity"] = int64(99)
		again, _ := clusterMachinePools(obj)
		qty, _, _ := unstructured.NestedInt64(again[0].(map[string]any), "quantity")
		assert.Equal(t, int64(3), qty)
	})
}

func TestFindMachinePool(t *testing.T) {
	pools := []any{
		familyPool("pool1", 3, false, false, true, nil, nil),
		familyPool("pool2", 1, true, true, false, nil, nil),
	}
	idx, ok := findMachinePool(pools, "pool2")
	assert.True(t, ok)
	assert.Equal(t, 1, idx)

	_, ok = findMachinePool(pools, "missing")
	assert.False(t, ok)
}

func TestPoolAutoscaled(t *testing.T) {
	assert.False(t, poolAutoscaled(familyPool("p", 1, false, false, true, nil, nil)))
	assert.True(t, poolAutoscaled(familyPool("p", 1, false, false, true, new(int64(1)), new(int64(3)))))
	// Half-set (should the write-side CEL ever be bypassed) is still autoscaled.
	assert.True(t, poolAutoscaled(familyPool("p", 1, false, false, true, new(int64(1)), nil)))
}

func TestPoolRequiresPositiveMin(t *testing.T) {
	assert.False(t, poolRequiresPositiveMin(familyPool("p", 1, false, false, true, nil, nil)))
	assert.True(t, poolRequiresPositiveMin(familyPool("p", 1, true, false, false, nil, nil)))
	assert.True(t, poolRequiresPositiveMin(familyPool("p", 1, false, true, false, nil, nil)))
}

func TestAutoscalingPaused(t *testing.T) {
	assert.False(t, autoscalingPaused(familyClusterObj("c", "1", nil, false)))
	assert.True(t, autoscalingPaused(familyClusterObj("c", "1", nil, true)))
}
