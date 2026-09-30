package provisioning

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// TestScaleUpIsAlwaysDestructive asserts that provisioningcluster is a
// Sensitive kind, so every scale is Destructive regardless of
// direction - the same gate a generic patch on the same object already
// demands.
func TestScaleUpIsAlwaysDestructive(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "5", []any{familyPool("pool1", 2, false, false, true, nil, nil)}, false)}
	cmd := NewScale(fakeFactory{c: client})

	eff, exec, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1", "nodes": "5"}, a: []string{"c1"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, safety.ActionUpdate, eff.Action)
	assert.True(t, eff.Destructive, "provisioningcluster is Sensitive: every scale is Destructive")
	assert.Equal(t, "5", eff.ResourceVersion)
	require.NotEmpty(t, eff.Diff)

	require.Len(t, client.updateDryRuns, 1)
	assert.True(t, client.updateDryRuns[0])
	pools, _ := clusterMachinePools(client.updated)
	gotQty, _, _ := unstructured.NestedInt64(pools[0].(map[string]any), "quantity")
	assert.Equal(t, int64(5), gotQty)
}

// TestScaleMissingQuantityWarnsUnknown asserts that a pool with no recorded
// quantity never silently reads as a current count of zero: the scale
// still succeeds (Destructive regardless, per the Sensitive-kind rule
// above), but the Effect names the current count as unknown.
func TestScaleMissingQuantityWarnsUnknown(t *testing.T) {
	t.Parallel()
	pool := familyPool("pool1", 0, false, false, true, nil, nil)
	delete(pool, "quantity")
	client := &familyClient{obj: familyClusterObj("c1", "1", []any{pool}, false)}
	cmd := NewScale(fakeFactory{c: client})

	eff, exec, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1", "nodes": "3"}, a: []string{"c1"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.True(t, eff.Destructive)
	require.NotEmpty(t, eff.Warnings)
	assert.Contains(t, eff.Warnings[0], "current count is unknown")
}

func TestScaleDownIsDestructive(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "5", []any{familyPool("pool1", 5, false, false, true, nil, nil)}, false)}
	cmd := NewScale(fakeFactory{c: client})

	eff, _, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1", "nodes": "2"}, a: []string{"c1"}})
	require.NoError(t, err)
	assert.True(t, eff.Destructive)
}

func TestScaleToZeroIsAllowedNoFloor(t *testing.T) {
	t.Parallel()
	// A control-plane/etcd pool has no invented floor: --nodes 0 is
	// schema-legal, the Destructive gate is the only protection.
	client := &familyClient{obj: familyClusterObj("c1", "1", []any{familyPool("pool1", 3, true, true, false, nil, nil)}, false)}
	cmd := NewScale(fakeFactory{c: client})

	eff, exec, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1", "nodes": "0"}, a: []string{"c1"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.True(t, eff.Destructive)
}

func TestScaleInvalidNodesRejectsBeforeAnyFetch(t *testing.T) {
	t.Parallel()
	for _, nodes := range []string{"-1", "abc", ""} {
		client := &familyClient{obj: familyClusterObj("c1", "1", []any{familyPool("pool1", 1, false, false, true, nil, nil)}, false)}
		cmd := NewScale(fakeFactory{c: client})

		_, exec, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1", "nodes": nodes}, a: []string{"c1"}})
		require.Error(t, err, "nodes=%q", nodes)
		assert.Nil(t, exec)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Nil(t, client.updated, "an invalid --nodes must never reach the fetch/update path")
	}
}

func TestScalePoolLessClusterRefuses(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("imported-1", "1", nil, false)}
	cmd := NewScale(fakeFactory{c: client})

	_, exec, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1", "nodes": "3"}, a: []string{"imported-1"}})
	require.Error(t, err)
	assert.Nil(t, exec)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "no machine pools")
	assert.Contains(t, ce.Message, "cluster scale")
}

// TestScalePoolLessClusterFieldPresentButEmptyRefuses asserts that a cluster
// whose machinePools field is present but currently empty gets a distinct
// refusal from a wholly pool-less one - it is not an imported/custom
// shape, so the message must not claim to be.
func TestScalePoolLessClusterFieldPresentButEmptyRefuses(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1", []any{}, false)}
	cmd := NewScale(fakeFactory{c: client})

	_, exec, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1", "nodes": "3"}, a: []string{"c1"}})
	require.Error(t, err)
	assert.Nil(t, exec)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "no machine pools")
	assert.NotContains(t, ce.Message, "imported and custom clusters carry none")
}

func TestScalePoolNotFoundRefuses(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1", []any{familyPool("pool1", 1, false, false, true, nil, nil)}, false)}
	cmd := NewScale(fakeFactory{c: client})

	_, _, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "missing", "nodes": "3"}, a: []string{"c1"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, `"missing"`)
	assert.Contains(t, ce.Hint, "inspect cluster")
}

func TestScaleAutoscaledUnpausedPoolRefuses(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1",
		[]any{familyPool("pool1", 2, false, false, true, new(int64(1)), new(int64(5)))}, false)}
	cmd := NewScale(fakeFactory{c: client})

	_, exec, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1", "nodes": "3"}, a: []string{"c1"}})
	require.Error(t, err)
	assert.Nil(t, exec)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "cluster autoscaler")
	assert.Contains(t, ce.Hint, "cluster pause-autoscaling")
	assert.Nil(t, client.updated, "the guard must refuse pre-Plan, before any Update dry-run")
}

func TestScaleAutoscaledPausedPoolIsAllowed(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1",
		[]any{familyPool("pool1", 2, false, false, true, new(int64(1)), new(int64(5)))}, true)}
	cmd := NewScale(fakeFactory{c: client})

	eff, exec, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1", "nodes": "3"}, a: []string{"c1"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.True(t, eff.Destructive, "provisioningcluster is Sensitive: every scale is Destructive")
}

func TestScaleClusterArgRequired(t *testing.T) {
	t.Parallel()
	cmd := NewScale(fakeFactory{c: &familyClient{}})
	_, _, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1", "nodes": "3"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}
