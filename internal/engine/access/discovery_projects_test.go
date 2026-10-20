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

// projectKey is the label and annotation a namespace carries for its
// project.
const projectKey = "field.cattle.io/projectId"

// steveNamespace is a steve namespace item on cluster: the project label
// holds the bare id and the annotation <cluster>:<project>; a namespace in
// no project has neither.
func steveNamespace(cluster, name, project string) map[string]any {
	meta := map[string]any{"name": name, "labels": map[string]any{"kubernetes.io/metadata.name": name}}
	if project != "" {
		meta["labels"].(map[string]any)[projectKey] = project
		meta["annotations"] = map[string]any{projectKey: cluster + ":" + project}
	}
	return map[string]any{"id": name, "type": "namespace", "metadata": meta}
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

// TestListNamespacesReadsTheClusterCollectionAndTheProjectAnnotation
// asserts the project of a namespace is the project its annotation names on
// this cluster: a label alone, an annotation for another cluster and an
// item steve serves without get (no labels) give the project the
// annotation names (or none), never the label's.
func TestListNamespacesReadsTheClusterCollectionAndTheProjectAnnotation(t *testing.T) {
	t.Parallel()
	withProjectLabel := steveNamespace("c-m-a", "with-project-label", "p-old")
	delete(withProjectLabel["metadata"].(map[string]any), "annotations")
	elsewhere := steveNamespace("c-other", "elsewhere", "p-else")
	withoutGet := map[string]any{"id": "without-get", "type": "namespace", "metadata": map[string]any{
		"name": "without-get", "labels": map[string]any{}, "annotations": map[string]any{projectKey: "c-m-a:p-sys"}}}
	srv, paths := pagedServerAt(t, steveNamespacesPath("c-m-a"), map[string]stevePage{
		"": {Data: []map[string]any{
			steveNamespace("c-m-a", "kube-system", "p-sys"), steveNamespace("c-m-a", "default", "p-def"), steveNamespace("c-m-a", "local", ""),
			withProjectLabel, elsewhere, withoutGet,
		}, Count: new(6), Revision: "44"},
	})
	rows, err := listNamespaces(t.Context(), fakeRootClient{srv}, "c-m-a")
	require.NoError(t, err)
	assert.Equal(t, []discoveredNamespace{
		{Name: "default", Project: "p-def"},
		{Name: "elsewhere", Project: ""},
		{Name: "kube-system", Project: "p-sys"},
		{Name: "local", Project: ""},
		{Name: "with-project-label", Project: ""},
		{Name: "without-get", Project: "p-sys"},
	}, rows)
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
