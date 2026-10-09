package access

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func steveProject(cluster, project, display string) map[string]any {
	return map[string]any{"id": cluster + "/" + project, "type": "management.cattle.io.project",
		"metadata": map[string]any{"name": project},
		"spec":     map[string]any{"clusterName": cluster, "displayName": display}}
}

func steveNamespace(name, project string) map[string]any {
	labels := map[string]any{"kubernetes.io/metadata.name": name}
	if project != "" {
		labels[projectIDLabel] = project
	}
	return map[string]any{"id": name, "type": "namespace",
		"metadata": map[string]any{"name": name, "labels": labels,
			"annotations": map[string]any{projectIDLabel: "c-x:" + project}}}
}

// pagedServerAt serves pages for one collection path, keyed by the continue
// token the previous page announced, and records every request path.
func pagedServerAt(t *testing.T, path string, pages map[string]stevePage) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		page, ok := pages[r.URL.Query().Get("continue")]
		if !ok {
			http.Error(w, "no such page", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(srv.Close)
	return srv, &paths
}

// TestListProjectsOrdersByClusterThenProject asserts the access commands'
// project set is ordered by cluster then project, whatever order the steve
// collection answers in.
func TestListProjectsOrdersByClusterThenProject(t *testing.T) {
	t.Parallel()
	srv, paths := pagedServerAt(t, steveProjectsPath, map[string]stevePage{
		"": {Data: []map[string]any{
			steveProject("c-m-b", "p-2", "Default"), steveProject("local", "p-l", "System"),
			steveProject("c-m-b", "p-1", "System"), steveProject("c-m-a", "p-9", "Default"),
		}, Count: new(4), Revision: "3"},
	})
	rows, err := listProjects(t.Context(), fakeRootClient{srv})
	require.NoError(t, err)
	assert.Equal(t, []discoveredProject{
		{Cluster: "c-m-a", Project: "p-9", DisplayName: "Default"},
		{Cluster: "c-m-b", Project: "p-1", DisplayName: "System"},
		{Cluster: "c-m-b", Project: "p-2", DisplayName: "Default"},
		{Cluster: "local", Project: "p-l", DisplayName: "System"},
	}, rows)
	assert.Equal(t, []string{steveProjectsPath}, *paths)
}

func TestListNamespacesReadsTheClusterCollectionAndTheProjectLabel(t *testing.T) {
	t.Parallel()
	srv, paths := pagedServerAt(t, steveNamespacesPath("c-m-a"), map[string]stevePage{
		"": {Data: []map[string]any{
			steveNamespace("kube-system", "p-sys"), steveNamespace("default", "p-def"), steveNamespace("local", ""),
		}, Count: new(3), Revision: "44"},
	})
	rows, err := listNamespaces(t.Context(), fakeRootClient{srv}, "c-m-a")
	require.NoError(t, err)
	assert.Equal(t, []discoveredNamespace{
		{Name: "default", Project: "p-def"},
		{Name: "kube-system", Project: "p-sys"},
		{Name: "local", Project: ""},
	}, rows, "the label, never the annotation, names the project; an unlabeled namespace has none")
	assert.Equal(t, []string{"/k8s/clusters/c-m-a/v1/namespaces"}, *paths)
}

func TestListNamespacesProtocolErrorsAreTerminal(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		item map[string]any
		want string
	}{
		"no id":                   {map[string]any{"type": "namespace"}, "item 0 has no id"},
		"metadata.name disagrees": {map[string]any{"id": "a", "metadata": map[string]any{"name": "b"}}, `"a" names itself "b"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, _ := pagedServerAt(t, steveNamespacesPath("c-m-a"), map[string]stevePage{
				"": {Data: []map[string]any{tc.item}, Count: new(1), Revision: "1"},
			})
			_, err := listNamespaces(t.Context(), fakeRootClient{srv}, "c-m-a")
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestListNamespacesItemThatIsNotAnObjectIsTerminal(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[3],"count":1,"revision":"1"}`))
	}))
	t.Cleanup(srv.Close)
	_, err := listNamespaces(t.Context(), fakeRootClient{srv}, "c-m-a")
	require.ErrorContains(t, err, "item 0 is not an object")
}

func TestListNamespacesPassesTheRootErrorThrough(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	_, err := listNamespaces(t.Context(), fakeRootClient{srv}, "c-m-a")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
}

func TestProjectsWithinClusters(t *testing.T) {
	t.Parallel()
	clusters := []discoveredCluster{{ID: "c-m-a"}, {ID: "local"}}
	require.NoError(t, projectsWithinClusters([]discoveredProject{{Cluster: "c-m-a", Project: "p-1"}, {Cluster: "local", Project: "p-l"}}, clusters))
	err := projectsWithinClusters([]discoveredProject{{Cluster: "c-m-zz", Project: "p-1"}}, clusters)
	require.ErrorContains(t, err, "project c-m-zz/p-1 belongs to a cluster the cluster collection did not list")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.False(t, ce.Retryable)
}
