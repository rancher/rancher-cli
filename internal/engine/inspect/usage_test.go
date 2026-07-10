package inspect

import (
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
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
	projectKind := mustKind(t, "project")

	namespaces := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
		namespaceObj("ns2", map[string]any{"field.cattle.io/projectId": "p-x"}),
	}}
	// Namespace-scoped pod Lists are keyed only by GVR in the fake (the ns arg
	// is ignored), so both ns1 and ns2 pod Lists return this same set; the
	// aggregation still lands in the single "p-x" bucket either way.
	client := newFakeClient().
		withList(projectKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*projectFixtureObj("p-x", "web", "")}}).
		withList(nsKind.GVR, namespaces).
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("ns1", "p1", "10m", "10Mi", "20m", "20Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "project": "p-x"}})
	require.NoError(t, err)

	require.NotEmpty(t, client.listSelectors)
	assert.Contains(t, client.listSelectors, "field.cattle.io/projectId=p-x", "the namespace List call must carry the rendered project selector")
	rows, ok := res.Data.(usageRows)
	require.True(t, ok)
	require.Contains(t, rows, "p-x")
	assert.NotContains(t, rows, "ns1")
}

// TestUsageContextProjectScopesWithoutFlag proves usage reads --project via
// EffectiveProject (flag > context), not in.String directly: a context-set
// project with no --project flag must still scope usage — an unset flag
// falling through to the cluster-wide branch would reproduce the exact
// silent-scope-loss DR-004 removed --project for.
func TestUsageContextProjectScopesWithoutFlag(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")
	projectKind := mustKind(t, "project")
	client := newFakeClient().
		withList(projectKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*projectFixtureObj("p-x", "web", "")}}).
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{namespaceObj("ns1", nil)}}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("ns1", "p1", "10m", "10Mi", "20m", "20Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client, ctxProject: "p-x"})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc"}})
	require.NoError(t, err)
	rows, ok := res.Data.(usageRows)
	require.True(t, ok)
	assert.Contains(t, rows, "p-x", "the context project must scope usage exactly as the flag does")
}

func TestUsageProjectResolvesDisplayName(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")
	projectKind := mustKind(t, "project")

	client := newFakeClient().
		withList(projectKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*projectFixtureObj("p-x", "web", "")}}).
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{namespaceObj("ns1", nil)}}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{})

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "project": "web"}})
	require.NoError(t, err)

	rows, ok := res.Data.(usageRows)
	require.True(t, ok)
	assert.Contains(t, rows, "p-x", "a display name must resolve to the project id before grouping")
}

func TestUsageNamespaceScopesMetricsFetch(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	pods := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		*podWithResources("ns1", "p1", "100m", "100Mi", "200m", "200Mi"),
	}}
	client := newFakeClient().
		withListNS(podKind.GVR, "ns1", pods).
		withListNS(podMetricsGVR, "ns1", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			podMetricsWithUsage("ns1", "p1", "80m", "90Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "namespace": "ns1"}})
	require.NoError(t, err)

	assert.Empty(t, res.Warnings)
	rows, ok := res.Data.(usageRows)
	require.True(t, ok)
	assert.Equal(t, "80m", rows["ns1"].CPUUsage, "metrics List must be scoped to ns1, not cluster-wide")

	for _, ns := range client.listNamespaces {
		assert.Equal(t, "ns1", ns, "every List call in --namespace mode must be scoped, never cluster-wide")
	}
}

func TestUsageProjectUnknownProjectErrors(t *testing.T) {
	t.Parallel()
	projectKind := mustKind(t, "project")
	client := newFakeClient().withList(projectKind.GVR, &unstructured.UnstructuredList{})

	cmd := NewUsage(&fakeFactory{client: client})
	_, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "project": "p-typo"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
}

// TestUsageProjectEmptyMembersReturnsEmptyAggregate pins the empty-project
// consistency decision (get's behavior wins): an empty member set is a valid
// empty result, not an error — a typo'd project name already fails loudly at
// ResolveProject with NOT_FOUND, so an existing-but-empty project legitimately
// has nothing, exactly like `get pods --project empty` returns zero rows.
func TestUsageProjectEmptyMembersReturnsEmptyAggregate(t *testing.T) {
	t.Parallel()
	projectKind := mustKind(t, "project")
	nsKind := mustKind(t, "namespace")
	podKind := mustKind(t, "pod")
	client := newFakeClient().
		withList(projectKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*projectFixtureObj("p-x", "web", "")}}).
		withList(nsKind.GVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{})

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "project": "p-x"}})
	require.NoError(t, err)
	rows, ok := res.Data.(usageRows)
	require.True(t, ok)
	require.Contains(t, rows, "p-x")
	assert.Equal(t, "0", rows["p-x"].CPURequest)
}

func TestUsageProjectRejectsInvalidLabelValue(t *testing.T) {
	t.Parallel()
	projectKind := mustKind(t, "project")
	malformed := "p-x,foo=bar"
	client := newFakeClient().
		withList(projectKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*projectFixtureObj(malformed, "web", "")}})
	cmd := NewUsage(&fakeFactory{client: client})
	_, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "project": malformed}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	for _, sel := range client.listSelectors {
		assert.NotContains(t, sel, malformed, "an invalid project value must never reach a List selector")
	}
}

func TestUsageNamespaceAndProjectValidatesMembership(t *testing.T) {
	t.Parallel()
	projectKind := mustKind(t, "project")
	nsKind := mustKind(t, "namespace")
	client := newFakeClient().
		withList(projectKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*projectFixtureObj("p-x", "web", "")}}).
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{namespaceObj("ns1", nil)}})

	cmd := NewUsage(&fakeFactory{client: client})
	_, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "namespace": "ns2", "project": "p-x"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "ns2")
	assert.Contains(t, ce.Message, "p-x")
}

func TestUsageNamespaceAndProjectMemberScopesAsNamespace(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	projectKind := mustKind(t, "project")
	nsKind := mustKind(t, "namespace")
	client := newFakeClient().
		withList(projectKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{*projectFixtureObj("p-x", "web", "")}}).
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{namespaceObj("ns1", nil)}}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withListNS(podKind.GVR, "ns1", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("ns1", "p1", "10m", "10Mi", "20m", "20Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "namespace": "ns1", "project": "p-x"}})
	require.NoError(t, err)
	rows, ok := res.Data.(usageRows)
	require.True(t, ok)
	assert.Contains(t, rows, "ns1", "a member namespace scopes the result by namespace, not by project bucket")
	assert.NotContains(t, rows, "p-x")
}
