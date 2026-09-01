package provisioning

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSetAutoscalingCapability(t *testing.T) {
	t.Parallel()
	d := NewSetAutoscaling(nil).Describe()
	assert.Equal(t, runtime.CapabilityPrime, d.Capability.Tier)
	assert.Equal(t, "cluster-autoscaling", d.Capability.Feature)
}

func TestSetAutoscalingSetsBounds(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1", []any{familyPool("pool1", 2, false, false, true, nil, nil)}, false)}
	cmd := NewSetAutoscaling(fakeFactory{c: client})

	eff, exec, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1", "min": "2", "max": "5"}, a: []string{"c1"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.True(t, eff.Destructive, "provisioningcluster is Sensitive: every family mutation is Destructive")
	assert.Contains(t, eff.Warnings[0], "managed by the cluster autoscaler")

	pools, _ := clusterMachinePools(client.updated)
	pool := pools[0].(map[string]any)
	min, _, _ := unstructured.NestedInt64(pool, "autoscalingMinSize")
	max, _, _ := unstructured.NestedInt64(pool, "autoscalingMaxSize")
	assert.Equal(t, int64(2), min)
	assert.Equal(t, int64(5), max)
}

func TestSetAutoscalingRejectsNonIntegers(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1", []any{familyPool("pool1", 2, false, false, true, nil, nil)}, false)}
	cmd := NewSetAutoscaling(fakeFactory{c: client})

	_, _, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1", "min": "x", "max": "5"}, a: []string{"c1"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Nil(t, client.updated)
}

func TestSetAutoscalingRejectsMinGreaterThanMax(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1", []any{familyPool("pool1", 2, false, false, true, nil, nil)}, false)}
	cmd := NewSetAutoscaling(fakeFactory{c: client})

	_, _, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1", "min": "5", "max": "2"}, a: []string{"c1"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "<=")
}

// TestSetAutoscalingRejectsNegativeMinOrMax pins that a negative bound is
// always rejected client-side, on any pool shape — sizes are counts, never
// negative, regardless of role.
func TestSetAutoscalingRejectsNegativeMinOrMax(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ min, max string }{
		{"-5", "-1"},
		{"-1", "3"},
		{"1", "-3"},
	} {
		client := &familyClient{obj: familyClusterObj("c1", "1", []any{familyPool("workers", 3, false, false, true, nil, nil)}, false)}
		cmd := NewSetAutoscaling(fakeFactory{c: client})
		_, exec, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "workers", "min": tc.min, "max": tc.max}, a: []string{"c1"}})
		require.Error(t, err, "min=%s max=%s", tc.min, tc.max)
		assert.Nil(t, exec)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, ">= 0")
		assert.Nil(t, client.updated, "a negative bound must never reach the fetch/update path")
	}
}

func TestSetAutoscalingRejectsNonPositiveMinOnControlPlaneOrEtcdPool(t *testing.T) {
	t.Parallel()
	for _, pool := range []map[string]any{
		familyPool("cp", 3, false, true, false, nil, nil),
		familyPool("etcd", 3, true, false, false, nil, nil),
	} {
		client := &familyClient{obj: familyClusterObj("c1", "1", []any{pool}, false)}
		cmd := NewSetAutoscaling(fakeFactory{c: client})
		_, _, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": pool["name"].(string), "min": "0", "max": "5"}, a: []string{"c1"}})
		require.Error(t, err, "pool %v", pool)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.Contains(t, ce.Message, "control-plane or etcd role")
	}
}

func TestSetAutoscalingAllowsZeroMinOnWorkerOnlyPool(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1", []any{familyPool("worker1", 3, false, false, true, nil, nil)}, false)}
	cmd := NewSetAutoscaling(fakeFactory{c: client})
	_, exec, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "worker1", "min": "0", "max": "5"}, a: []string{"c1"}})
	require.NoError(t, err)
	assert.NotNil(t, exec)
}

func TestSetAutoscalingPoolLessClusterRefuses(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("imported-1", "1", nil, false)}
	cmd := NewSetAutoscaling(fakeFactory{c: client})
	_, _, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1", "min": "1", "max": "5"}, a: []string{"imported-1"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Message, "no machine pools")
}

func TestClearAutoscalingCapability(t *testing.T) {
	t.Parallel()
	d := NewClearAutoscaling(nil).Describe()
	assert.Equal(t, runtime.CapabilityPrime, d.Capability.Tier)
	assert.Equal(t, "cluster-autoscaling", d.Capability.Feature)
}

func TestClearAutoscalingRemovesBounds(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1",
		[]any{familyPool("pool1", 4, false, false, true, new(int64(2)), new(int64(5)))}, false)}
	cmd := NewClearAutoscaling(fakeFactory{c: client})

	eff, exec, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1"}, a: []string{"c1"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.True(t, eff.Destructive, "provisioningcluster is Sensitive: every family mutation is Destructive")

	pools, _ := clusterMachinePools(client.updated)
	pool := pools[0].(map[string]any)
	_, hasMin := pool["autoscalingMinSize"]
	_, hasMax := pool["autoscalingMaxSize"]
	assert.False(t, hasMin)
	assert.False(t, hasMax)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Contains(t, res.Text, "left at manual quantity 4")
}

// TestClearAutoscalingOnUnmanagedPoolIsNoOp pins the design's convergence
// contract: a pool that already carries neither bound succeeds, naming
// nothing-to-clear rather than surfacing a raw path-not-found error.
func TestClearAutoscalingOnUnmanagedPoolIsNoOp(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1", []any{familyPool("pool1", 4, false, false, true, nil, nil)}, false)}
	cmd := NewClearAutoscaling(fakeFactory{c: client})

	eff, exec, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1"}, a: []string{"c1"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.True(t, eff.Destructive, "provisioningcluster is Sensitive: every family mutation is Destructive")

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Contains(t, res.Text, "already manual; nothing to clear")
}

// TestClearAutoscalingHalfSetPoolClearsCleanly pins that clear removes
// whichever of the two fields is actually present, even when the write-side
// both-or-neither CEL is somehow bypassed.
func TestClearAutoscalingHalfSetPoolClearsCleanly(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1",
		[]any{familyPool("pool1", 4, false, false, true, new(int64(2)), nil)}, false)}
	cmd := NewClearAutoscaling(fakeFactory{c: client})

	_, exec, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "pool1"}, a: []string{"c1"}})
	require.NoError(t, err)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Contains(t, res.Text, "left at manual quantity 4")
}

func TestClearAutoscalingPoolNotFoundRefuses(t *testing.T) {
	t.Parallel()
	client := &familyClient{obj: familyClusterObj("c1", "1", []any{familyPool("pool1", 1, false, false, true, nil, nil)}, false)}
	cmd := NewClearAutoscaling(fakeFactory{c: client})
	_, _, err := planFamily(t, cmd, ccInput{s: map[string]string{"pool": "missing"}, a: []string{"c1"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}
