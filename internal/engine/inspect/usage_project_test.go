package inspect

import (
	"maps"
	"net/http"
	"slices"
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

	root := projectsRoot(t, "c-abc", steveProjectItem("c-abc", "p-x", "web"), steveProjectItem("c-abc", "p-y", "api"))
	client := newFakeClient().
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

	cmd := NewUsage(&fakeFactory{client: client}, root.roots())
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

	// p-x→"web", p-y→"api": ids sort p-x < p-y but display names sort api < web.
	// Rows must be ordered by the displayed scope (display name), not by project id.
	apiIdx, webIdx := -1, -1
	for i, s := range scopes {
		switch s {
		case "api":
			apiIdx = i
		case "web":
			webIdx = i
		}
	}
	require.NotEqual(t, -1, apiIdx, "api must appear in scopes")
	require.NotEqual(t, -1, webIdx, "web must appear in scopes")
	assert.Less(t, apiIdx, webIdx, "rows sorted by display name (api < web), not by project id (p-x < p-y)")
}

func TestUsageByProjectWithProjectFlagSingleBucket(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")

	client := newFakeClient().
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
		}}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("ns1", "p1", "100m", "128Mi", "200m", "256Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client}, newSteveRoot(t, nil).roots())
	ctx := withProject(t, "c-abc", "p-x", "web", "ns1")
	res, err := execReadCtx(t, ctx, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "project", "project": "p-x"}})
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

	root := projectsRoot(t, "c-abc")
	client := newFakeClient().
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			namespaceObj("kube-system", nil),
		}}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("kube-system", "coredns", "100m", "70Mi", "200m", "140Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client}, root.roots())
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

	root := projectsRoot(t, "c-abc", steveProjectItem("c-abc", "p-x", "web"))
	client := newFakeClient().
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
			namespaceObj("ns2", map[string]any{"field.cattle.io/projectId": "p-x"}),
		}}).
		withListNS(podMetricsGVR, "ns1", &unstructured.UnstructuredList{}).
		withListNS(podKind.GVR, "ns1", &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("ns1", "p1", "100m", "128Mi", "200m", "256Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client}, root.roots())
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

	client := newFakeClient().
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			namespaceObj("ns-other", map[string]any{"field.cattle.io/projectId": "p-y"}),
		}})

	cmd := NewUsage(&fakeFactory{client: client}, newSteveRoot(t, nil).roots())
	_, err := execReadCtx(t, withProject(t, "c-abc", "p-x", "web"), cmd, input{s: map[string]string{
		"cluster": "c-abc", "by": "project", "project": "p-x", "namespace": "ns-other",
	}})
	require.Error(t, err, "--namespace outside --project must be NotMemberError under --by project too")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "is not in project")
}

// --node under --by project narrows request/limit sums to that node's pods.
func TestUsageByProjectNodeFilter(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")

	root := projectsRoot(t, "c-abc", steveProjectItem("c-abc", "p-x", "web"))
	client := newFakeClient().
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
		}}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podOnNode("ns1", "p1", "n1", "100m", "128Mi", "200m", "256Mi"),
			*podOnNode("ns1", "p2", "n2", "50m", "64Mi", "100m", "128Mi"),
		}})

	cmd := NewUsage(&fakeFactory{client: client}, root.roots())
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "project", "node": "n1"}})
	require.NoError(t, err)

	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	require.Contains(t, env.Data, "p-x")
	assert.Equal(t, "100m", env.Data["p-x"].CPURequest, "only n1's pods count under --node n1")
}

// TestUsageByProjectWithProjectReadsNoProjectCollection asserts --by project
// --project builds its one bucket from the project the pipeline resolved:
// no projects collection read, and the bucket is named by the project's
// display name, or by its id when the display name is empty.
func TestUsageByProjectWithProjectReadsNoProjectCollection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, display, scope string }{
		{"display name", "web", "web"},
		{"no display name", "", "p-x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			podKind := mustKind(t, "pod")
			nsKind := mustKind(t, "namespace")
			client := newFakeClient().
				withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
					namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
				}}).
				withList(podMetricsGVR, &unstructured.UnstructuredList{}).
				withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
					*podWithResources("ns1", "p1", "100m", "128Mi", "200m", "256Mi"),
				}})
			root := projectsRoot(t, "c-abc", steveProjectItem("c-abc", "p-x", "other"))

			cmd := NewUsage(&fakeFactory{client: client}, root.roots())
			ctx := withProject(t, "c-abc", "p-x", tc.display, "ns1")
			res, err := execReadCtx(t, ctx, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "project", "project": "p-x"}})
			require.NoError(t, err)

			assert.Empty(t, root.requests(), "no projects collection read with --project")
			env, ok := res.Data.(usageEnvelope)
			require.True(t, ok)
			require.Equal(t, []string{"p-x"}, slices.Collect(maps.Keys(env.Data)))
			tabular, ok := res.Data.(interface{ Rows() [][]string })
			require.True(t, ok)
			require.Len(t, tabular.Rows(), 1)
			assert.Equal(t, tc.scope, tabular.Rows()[0][0])
		})
	}
}

// TestUsageByProjectWithProjectSumsOnlyItsPods asserts the one bucket of
// --by project --project holds the sum of that project's pods only: pods of
// another project's namespace stay out of it and get no row.
func TestUsageByProjectWithProjectSumsOnlyItsPods(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")
	client := newFakeClient().
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
			namespaceObj("ns2", map[string]any{"field.cattle.io/projectId": "p-y"}),
		}}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			*podWithResources("ns1", "p1", "100m", "128Mi", "200m", "256Mi"),
			*podWithResources("ns1", "p2", "50m", "64Mi", "100m", "128Mi"),
			*podWithResources("ns2", "p3", "300m", "512Mi", "600m", "1Gi"),
		}})
	root := projectsRoot(t, "c-abc")
	cmd := NewUsage(&fakeFactory{client: client}, root.roots())
	ctx := withProject(t, "c-abc", "p-x", "web", "ns1")
	res, err := execReadCtx(t, ctx, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "project", "project": "web"}})
	require.NoError(t, err)
	env, ok := res.Data.(usageEnvelope)
	require.True(t, ok)
	require.Equal(t, []string{"p-x"}, slices.Collect(maps.Keys(env.Data)))
	assert.Equal(t, "150m", env.Data["p-x"].CPURequest)
	assert.Equal(t, "300m", env.Data["p-x"].CPULimit)
	assert.Equal(t, "192Mi", env.Data["p-x"].MemoryRequest)
	assert.Equal(t, "384Mi", env.Data["p-x"].MemoryLimit)
}

// TestUsageByProjectReadsTheClustersProjectsOnce asserts --by project
// without --project names its buckets from one read of the resolved
// cluster's steve projects collection.
func TestUsageByProjectReadsTheClustersProjectsOnce(t *testing.T) {
	t.Parallel()
	podKind := mustKind(t, "pod")
	nsKind := mustKind(t, "namespace")
	client := newFakeClient().
		withList(nsKind.GVR, &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
			namespaceObj("ns1", map[string]any{"field.cattle.io/projectId": "p-x"}),
		}}).
		withList(podMetricsGVR, &unstructured.UnstructuredList{}).
		withList(podKind.GVR, &unstructured.UnstructuredList{})
	root := projectsRoot(t, "c-abc", steveProjectItem("c-abc", "p-x", "web"), steveProjectItem("c-abc", "p-y", "api"))

	cmd := NewUsage(&fakeFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}, root.roots())
	res, err := execRead(t, cmd, input{s: map[string]string{"cluster": "prod", "by": "project"}})
	require.NoError(t, err)

	assert.Equal(t, []string{steveProjectsPath("c-abc")}, root.requests())
	tabular, ok := res.Data.(interface{ Rows() [][]string })
	require.True(t, ok)
	scopes := make([]string, 0, len(tabular.Rows()))
	for _, row := range tabular.Rows() {
		scopes = append(scopes, row[0])
	}
	assert.Equal(t, []string{"api", "web"}, scopes, "every listed project gets a row named by its display name")
}

// TestUsageByProjectProjectsCollection403IsRBAC asserts a refused projects
// collection read is the command's error, worded as the steve read's
// refusal.
func TestUsageByProjectProjectsCollection403IsRBAC(t *testing.T) {
	t.Parallel()
	nsKind := mustKind(t, "namespace")
	client := newFakeClient().
		withList(nsKind.GVR, &unstructured.UnstructuredList{})
	root := newSteveRoot(t, map[string]steveAnswer{steveProjectsPath("c-abc"): {status: http.StatusForbidden}})

	cmd := NewUsage(&fakeFactory{client: client}, root.roots())
	_, err := execRead(t, cmd, input{s: map[string]string{"cluster": "c-abc", "by": "project"}})
	var cerr *clierr.Error
	require.ErrorAs(t, err, &cerr)
	assert.Equal(t, clierr.CodeRBAC, cerr.Code)
	assert.Equal(t, "the server refused GET /v1/management.cattle.io.projects/c-abc (403); Rancher answers this request with 200 for every signed-in user; a proxy in front of Rancher or an unsupported Rancher refused it", cerr.Message)
	assert.False(t, client.listed(nsKind.GVR), "no namespace read after the refusal")
}
