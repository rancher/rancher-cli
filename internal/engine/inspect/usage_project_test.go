package inspect

import (
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestUsageByProjectGroupsByProject(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")
	projectKind := mustKind(t, "project")

	client := newFakeClient().
		withList(projectKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*projectFixtureObj("p-x", "web", ""),
			*projectFixtureObj("p-y", "api", ""),
		}}).
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
			namespaceObj("ns2", map[string]any{"field.cattle.io/projectId": "p-y"}),
			namespaceObj("ns3", nil), // unowned
		}}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("ns1", "p1", "100m", "128Mi", "200m", "256Mi"),
			*podWithResources("ns2", "p2", "50m", "64Mi", "100m", "128Mi"),
			*podWithResources("ns3", "p3", "10m", "10Mi", "20m", "20Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "project"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	assert.Equal(t, "project", env.By)
	require.Contains(t, env.Data, "p-x", "row key is project id")
	require.Contains(t, env.Data, "p-y")
	require.Contains(t, env.Data, "<none>", "pods in unowned namespaces must appear under <none>")
	assert.Equal(t, "100m", env.Data["p-x"].CPURequest)
	assert.Equal(t, "50m", env.Data["p-y"].CPURequest)
	assert.Equal(t, "10m", env.Data["<none>"].CPURequest)

	tabular, ok := res.Data.(interface{ Rows() [][]string })
	require.True(t, ok)
	scopes := make([]string, 0, len(tabular.Rows()))
	for _, row := range tabular.Rows() {
		scopes = append(scopes, row[0])
	}
	assert.Contains(t, scopes, "web", "table SCOPE shows the project display name, not the id")
	assert.Contains(t, scopes, "api")
	assert.NotContains(t, scopes, "p-x")
}

func TestUsageByProjectWithProjectFlagSingleBucket(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")
	projectKind := mustKind(t, "project")

	client := newFakeClient().
		withList(projectKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*projectFixtureObj("p-x", "web", ""),
		}}).
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
		}}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("ns1", "p1", "100m", "128Mi", "200m", "256Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "project", "project": "p-x"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	assert.Len(t, env.Data, 1, "--by project --project X must return exactly one bucket")
	require.Contains(t, env.Data, "p-x")
}

func TestUsageByProjectUnownedPodsInNoneRow(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")
	projectKind := mustKind(t, "project")

	client := newFakeClient().
		withList(projectKind.GVR, &unstructured.UnstructuredList{}).
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			namespaceObj("kube-system", nil),
		}}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("kube-system", "coredns", "100m", "70Mi", "200m", "140Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "project"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	require.Contains(t, env.Data, "<none>")
	assert.Equal(t, "100m", env.Data["<none>"].CPURequest)
}

// --namespace under --by project narrows to that namespace's pods; with
// --project too, the namespace must be a member.
func TestUsageByProjectNamespaceFilter(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")
	projectKind := mustKind(t, "project")

	client := newFakeClient().
		withList(projectKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*projectFixtureObj("p-x", "web", ""),
		}}).
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
			namespaceObj("ns2", map[string]any{"field.cattle.io/projectId": "p-x"}),
		}}).
		withListNS(podMetricsGVR, "ns1", &unstructured.UnstructuredList{}).
		withListNS(podKind.GVR, "ns1", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("ns1", "p1", "100m", "128Mi", "200m", "256Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "project", "namespace": "ns1"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	require.Contains(t, env.Data, "p-x")
	assert.Equal(t, "100m", env.Data["p-x"].CPURequest, "only ns1's pods count under --namespace ns1")
}

func TestUsageByProjectNamespaceMustBeMember(t *testing.T) {
	t.Parallel()
	nsKind := mustKind(t, "namespace")
	projectKind := mustKind(t, "project")

	client := newFakeClient().
		withList(projectKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*projectFixtureObj("p-x", "web", ""),
		}}).
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			namespaceObj("ns-other", map[string]any{"field.cattle.io/projectId": "p-y"}),
		}})

	cmd := NewUsage(&fakeFactory{client: client})
	_, err := execRead(t, cmd, input{s: map[string]string{
		"cluster": "c-abc", "by": "project", "project": "p-x", "namespace": "ns-other",
	}})
	require.Error(t, err, "--namespace outside --project must be NotMemberError under --by project too")
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "is not in project")
}

// --node under --by project narrows request/limit sums to that node's pods.
func TestUsageByProjectNodeFilter(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")
	projectKind := mustKind(t, "project")

	client := newFakeClient().
		withList(projectKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*projectFixtureObj("p-x", "web", ""),
		}}).
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
		}}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podOnNode("ns1", "p1", "n1", "100m", "128Mi", "200m", "256Mi"),
			*podOnNode("ns1", "p2", "n2", "50m", "64Mi", "100m", "128Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client})
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "project", "node": "n1"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	require.Contains(t, env.Data, "p-x")
	assert.Equal(t, "100m", env.Data["p-x"].CPURequest, "only n1's pods count under --node n1")
}
