package inspect

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func podWithResources(ns, name, cpuReq, memReq, cpuLim, memLim string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"containers": []any{
				map[string]any{
					"name": "app",
					"resources": map[string]any{
						"requests": map[string]any{"cpu": cpuReq, "memory": memReq},
						"limits":   map[string]any{"cpu": cpuLim, "memory": memLim},
					},
				},
			},
		},
	}}
}

func podMetricsWithUsage(ns, name, cpu, memory string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "namespace": ns},
		"containers": []any{
			map[string]any{"name": "app", "usage": map[string]any{"cpu": cpu, "memory": memory}},
		},
	}}
}

func namespaceObj(name string, labels map[string]any) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "labels": labels},
	}}
}

func TestUsageAggregatesPerNamespace(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")

	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithResources("ns1", "p1", "100m", "100Mi", "200m", "200Mi"),
		*podWithResources("ns2", "p1", "50m", "50Mi", "100m", "100Mi"),
	}}
	metrics := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		podMetricsWithUsage("ns1", "p1", "80m", "90Mi"),
		podMetricsWithUsage("ns2", "p1", "40m", "45Mi"),
	}}

	client := newFakeClient().
		withList(podKind.GVR, pods).
		withList(podMetricsGVR, metrics)

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc"}})
	require.NoError(t, err)

	assert.Equal(t, "ResourceUsage", res.Kind)
	assert.Empty(t, res.Warnings)
	rows, ok := res.Data.(usageRows)
	require.True(t, ok)
	require.Contains(t, rows, "ns1")
	require.Contains(t, rows, "ns2")
	assert.Equal(t, "100m", rows["ns1"].CPURequest)
	assert.Equal(t, "200Mi", rows["ns1"].MemoryLimit)
	assert.Equal(t, "80m", rows["ns1"].CPUUsage)
	assert.Equal(t, "50m", rows["ns2"].CPURequest)
	assert.Equal(t, "45Mi", rows["ns2"].MemoryUsage)
}

func TestUsageDegradesWithoutMetrics(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithResources("ns1", "p1", "100m", "100Mi", "200m", "200Mi"),
	}}
	client := newFakeClient().
		withList(podKind.GVR, pods).
		withListErr(podMetricsGVR, errors.New("no metrics server"))

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc"}})
	require.NoError(t, err)

	require.Len(t, res.Warnings, 1)
	assert.Equal(t, "metrics.k8s.io unavailable on cluster c-abc", res.Warnings[0])
	rows, ok := res.Data.(usageRows)
	require.True(t, ok)
	assert.Empty(t, rows["ns1"].CPUUsage)
}

func TestUsageProjectGroupsByLabel(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")

	namespaces := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
		namespaceObj("ns2", map[string]any{"field.cattle.io/projectId": "p-x"}),
	}}
	// Namespace-scoped pod Lists are keyed only by GVR in the fake (the ns arg
	// is ignored), so both ns1 and ns2 pod Lists return this same set; the
	// aggregation still lands in the single "p-x" bucket either way.
	client := newFakeClient().
		withList(nsKind.GVR, namespaces).
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("ns1", "p1", "10m", "10Mi", "20m", "20Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "project": "p-x"}})
	require.NoError(t, err)

	rows, ok := res.Data.(usageRows)
	require.True(t, ok)
	require.Contains(t, rows, "p-x")
	assert.NotContains(t, rows, "ns1")
}

func TestUsageNamespaceAndProjectConflict(t *testing.T) {
	t.Parallel()
	cmd := NewUsage(&fakeFactory{client: newFakeClient()})
	_, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "namespace": "ns1", "project": "p-x"}})
	require.Error(t, err)
}
