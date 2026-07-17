package inspect

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestUsageByPodOneRowPerPod(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")

	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithResources("ns1", "p1", "100m", "128Mi", "200m", "256Mi"),
		*podWithResources("ns2", "p2", "50m", "64Mi", "100m", "128Mi"),
	}}
	metrics := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		podMetricsWithUsage("ns1", "p1", "80m", "100Mi"),
	}}

	client := newFakeClient().
		withList(podKind.GVR, pods).
		withList(podMetricsGVR, metrics)

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "pod"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	assert.Equal(t, "pod", env.By)
	require.Contains(t, env.Data, "ns1/p1")
	require.Contains(t, env.Data, "ns2/p2")
	assert.Equal(t, "100m", env.Data["ns1/p1"].CPURequest)
	assert.Equal(t, "80m", env.Data["ns1/p1"].CPUUsage)
	assert.Equal(t, "0", env.Data["ns2/p2"].CPUUsage, "pod with no metrics entry reports zero usage, matching the namespace grouping's treatment")
}

func TestUsageByPodNodeFilter(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")

	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podOnNode("ns1", "p1", "n1", "100m", "128Mi", "200m", "256Mi"),
		*podOnNode("ns1", "p2", "n2", "50m", "64Mi", "100m", "128Mi"),
	}}

	client := newFakeClient().
		withList(podKind.GVR, pods).
		withList(podMetricsGVR, &unstructured.UnstructuredList{})

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "pod", "node": "n1"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	assert.Contains(t, env.Data, "ns1/p1")
	assert.NotContains(t, env.Data, "ns1/p2", "--node filter must exclude pods on other nodes")
}

func TestUsageByPodNamespaceFilter(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")

	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithResources("ns1", "p1", "100m", "128Mi", "200m", "256Mi"),
	}}

	client := newFakeClient().
		withListNS(podKind.GVR, "ns1", pods).
		withListNS(podMetricsGVR, "ns1", &unstructured.UnstructuredList{})

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "pod", "namespace": "ns1"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	assert.Contains(t, env.Data, "ns1/p1")
}
