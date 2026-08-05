package inspect

import (
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// nodeObj, nodeMetricsObj, and nodeMetricsObjPtr move here VERBATIM from
// topnode_test.go (which this task deletes) — without this move, deleting
// topnode_test.go breaks compilation of this file:

func nodeObj(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
	}}
}

func nodeMetricsObj(name, cpu, memory string) unstructured.Unstructured {
	return *nodeMetricsObjPtr(name, cpu, memory)
}

func nodeMetricsObjPtr(name, cpu, memory string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"usage":    map[string]any{"cpu": cpu, "memory": memory},
	}}
}

// podOnNode is NOT defined here — Task 2 already added it to usage_test.go;
// this file just uses it.

func TestUsageByNodeJoinsNodeMetrics(t *testing.T) {
	t.Parallel()
	nodeKind := mustKind(t, "node")
	podKind := mustKind(t, "pod")

	nodes := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*nodeObj("n1"), *nodeObj("n2")}}
	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podOnNode("ns1", "p1", "n1", "100m", "128Mi", "200m", "256Mi"),
	}}
	metrics := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		nodeMetricsObj("n1", "80m", "100Mi"),
	}}

	client := newFakeClient().
		withList(nodeKind.GVR, nodes).
		withList(podKind.GVR, pods).
		withList(nodeMetricsGVR, metrics)

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "node"}})
	require.NoError(t, err)

	assert.Equal(t, "ResourceUsage", res.Kind)
	assert.Empty(t, res.Warnings)
	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	assert.Equal(t, "node", env.By)
	require.Contains(t, env.Data, "n1")
	require.Contains(t, env.Data, "n2", "zero-pod node must appear with empty request/limit rows")
	assert.Equal(t, "100m", env.Data["n1"].CPURequest)
	assert.Equal(t, "128Mi", env.Data["n1"].MemoryRequest)
	assert.Equal(t, "80m", env.Data["n1"].CPUUsage, "USAGE must come from node metrics, not pod metrics")
	assert.Equal(t, "100Mi", env.Data["n1"].MemoryUsage)
	assert.Equal(t, "0", env.Data["n2"].CPURequest, "zero-pod node has zero requests")
	assert.Equal(t, "", env.Data["n2"].CPUUsage, "a node absent from metrics reports empty usage, not zero")
}

func TestUsageByNodeDegradesWithoutMetricsServer(t *testing.T) {
	t.Parallel()
	nodeKind := mustKind(t, "node")
	podKind := mustKind(t, "pod")
	nodes := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*nodeObj("n1")}}
	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{}}

	client := newFakeClient().
		withList(nodeKind.GVR, nodes).
		withList(podKind.GVR, pods).
		withListErr(nodeMetricsGVR, errors.New("no metrics server"))

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "node"}})
	require.NoError(t, err)

	require.Len(t, res.Warnings, 1)
	assert.Equal(t, "metrics.k8s.io unavailable on cluster c-abc", res.Warnings[0])
	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	assert.Empty(t, env.Data["n1"].CPUUsage)
}

func TestUsageByNodeSingleNode(t *testing.T) {
	t.Parallel()
	nodeKind := mustKind(t, "node")
	podKind := mustKind(t, "pod")

	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podOnNode("ns1", "p1", "n1", "50m", "64Mi", "100m", "128Mi"),
	}}
	client := newFakeClient().
		withObject(nodeKind.GVR, "", "n1", nodeObj("n1")).
		withObject(nodeMetricsGVR, "", "n1", nodeMetricsObjPtr("n1", "40m", "50Mi")).
		withList(podKind.GVR, pods)

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "node", "node": "n1"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	require.Contains(t, env.Data, "n1")
	assert.Equal(t, "50m", env.Data["n1"].CPURequest)
	assert.Equal(t, "40m", env.Data["n1"].CPUUsage)
}

func TestUsageByNodeSingleNodeDegradesWithoutMetricsServer(t *testing.T) {
	t.Parallel()
	nodeKind := mustKind(t, "node")
	podKind := mustKind(t, "pod")

	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podOnNode("ns1", "p1", "n1", "50m", "64Mi", "100m", "128Mi"),
	}}
	// No nodeMetricsGVR object registered — fake.Get returns error for missing object.
	client := newFakeClient().
		withObject(nodeKind.GVR, "", "n1", nodeObj("n1")).
		withList(podKind.GVR, pods)

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "node", "node": "n1"}})
	require.NoError(t, err)

	require.Len(t, res.Warnings, 1)
	assert.Equal(t, "metrics.k8s.io unavailable on cluster c-abc", res.Warnings[0])
	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	require.Contains(t, env.Data, "n1")
	assert.Empty(t, env.Data["n1"].CPUUsage)
	assert.Equal(t, "50m", env.Data["n1"].CPURequest)
}

func TestUsageByNodePendingPodsExcluded(t *testing.T) {
	t.Parallel()
	nodeKind := mustKind(t, "node")
	podKind := mustKind(t, "pod")

	// Pod with no spec.nodeName (pending, not yet scheduled).
	pendingPod := podWithResources("ns1", "pending", "200m", "256Mi", "400m", "512Mi")

	client := newFakeClient().
		withList(nodeKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*nodeObj("n1")}}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*pendingPod}}).
		withList(nodeMetricsGVR, &unstructured.UnstructuredList{})

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "node"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	assert.Equal(t, "0", env.Data["n1"].CPURequest, "pending pods (no nodeName) must not appear in node rows")
}

// TestUsageByNodeGetNotFoundCurated verifies that a hint-less NOT_FOUND from
// the single-node Get is curated to "node not found: <name>" and that a
// hint-bearing NOT_FOUND passes through unchanged.
func TestUsageByNodeGetNotFoundCurated(t *testing.T) {
	t.Parallel()
	nodeKind := mustKind(t, "node")
	podKind := mustKind(t, "pod")
	pods := &unstructured.UnstructuredList{}

	// hint-less 404 → curated shape (produced by the dynamic client; fake simulates it)
	client := newFakeClient().
		withGetErr(nodeKind.GVR, "", "n1", kinds.NotFoundCurated(nodeKind, "n1")).
		withList(podKind.GVR, pods)
	cmd := NewUsage(&fakeFactory{client: client})
	_, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "node", "node": "n1"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, "node not found: n1", ce.Message)
	assert.Equal(t, "list with: rancher get nodes", ce.Hint)

	// hint-bearing 404 passes through unchanged
	hinted := clierr.New(clierr.CodeNotFound, "unsupported group").WithHint("try another group")
	client2 := newFakeClient().
		withGetErr(nodeKind.GVR, "", "n1", hinted).
		withList(podKind.GVR, pods)
	cmd2 := NewUsage(&fakeFactory{client: client2})
	_, err2 := execRead(t, cmd2, input{s: map[string]string{"cluster": "c-abc", "by": "node", "node": "n1"}})
	var ce2 *clierr.Error
	require.ErrorAs(t, err2, &ce2)
	assert.Equal(t, "try another group", ce2.Hint, "hint-bearing error must pass through unchanged")
}

// A pod whose spec.nodeName names a node absent from the List (deleted mid-run
// or partial RBAC) must still produce a row — USAGE empty, REQUEST/LIMIT from
// the pod spec.
func TestUsageByNodeGhostNodeKeepsRow(t *testing.T) {
	t.Parallel()
	nodeKind := mustKind(t, "node")
	podKind := mustKind(t, "pod")

	nodes := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*nodeObj("n1")}}
	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podOnNode("ns1", "p1", "n1", "100m", "128Mi", "200m", "256Mi"),
		*podOnNode("ns1", "p2", "ghost-node", "50m", "64Mi", "100m", "128Mi"),
	}}
	metrics := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		nodeMetricsObj("n1", "80m", "100Mi"),
	}}

	client := newFakeClient().
		withList(nodeKind.GVR, nodes).
		withList(podKind.GVR, pods).
		withList(nodeMetricsGVR, metrics)

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "node"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	require.Contains(t, env.Data, "n1")
	require.Contains(t, env.Data, "ghost-node", "pod on a node absent from the List must still produce a row")
	assert.Equal(t, "50m", env.Data["ghost-node"].CPURequest, "ghost-node row carries the pod's CPU request")
	assert.Equal(t, "", env.Data["ghost-node"].CPUUsage, "ghost-node row has no metrics entry, so CPUUsage is empty")
}
