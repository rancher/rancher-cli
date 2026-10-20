package testrancher_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const projectsSeed = "seed:secret"

// getDecoded GETs url with bearer and decodes a JSON object body; a body
// that is not JSON decodes to nil.
func getDecoded(t *testing.T, srv *httptest.Server, path, bearer string) (int, map[string]any) {
	t.Helper()
	resp, body := doJSON(t, srv.Client(), http.MethodGet, srv.URL+path, nil, bearer)
	var out map[string]any
	if json.Unmarshal(body, &out) != nil {
		out = nil
	}
	return resp.StatusCode, out
}

// projectItems returns the data items of a decoded steve collection.
func projectItems(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	data, ok := body["data"].([]any)
	require.True(t, ok, "data is an array: %v", body)
	out := make([]map[string]any, 0, len(data))
	for _, d := range data {
		item, ok := d.(map[string]any)
		require.True(t, ok)
		out = append(out, item)
	}
	return out
}

// TestBuiltinProjectsCollections asserts that without ProjectVisibility the
// facade serves the local cluster's Default and System projects on the
// collection of every cluster and on the local cluster's, in steve's item
// shape, and an unknown cluster's collection as 200 with empty data and no
// count.
func TestBuiltinProjectsCollections(t *testing.T) {
	t.Parallel()
	fall := &recordingHandler{}
	_, srv := newFacade(t, testrancher.Options{SeedToken: projectsSeed}, fall)

	for _, path := range []string{testrancher.ProjectsCollectionPath, testrancher.ProjectsCollectionPath + "/local"} {
		status, body := getDecoded(t, srv, path, projectsSeed)
		require.Equal(t, http.StatusOK, status, path)
		assert.Equal(t, "collection", body["type"], path)
		assert.InDelta(t, 2, body["count"], 0, path)
		assert.NotContains(t, body, "continue", path)
		items := projectItems(t, body)
		require.Len(t, items, 2, path)
		for i, want := range []struct{ id, name, display string }{
			{"local/p-default", "p-default", "Default"},
			{"local/p-system", "p-system", "System"},
		} {
			assert.Equal(t, want.id, items[i]["id"], path)
			assert.Equal(t, "management.cattle.io.project", items[i]["type"], path)
			assert.Equal(t, "management.cattle.io/v3", items[i]["apiVersion"], path)
			assert.Equal(t, "Project", items[i]["kind"], path)
			assert.Equal(t, map[string]any{"name": want.name, "namespace": "local"}, items[i]["metadata"], path)
			assert.Equal(t, map[string]any{"clusterName": "local", "displayName": want.display}, items[i]["spec"], path)
		}
	}

	status, body := getDecoded(t, srv, testrancher.ProjectsCollectionPath+"/c-nosuchcluster", projectsSeed)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "collection", body["type"])
	assert.NotContains(t, body, "count", "an empty collection carries no count")
	assert.NotContains(t, body, "continue")
	assert.Empty(t, projectItems(t, body))

	assert.Empty(t, fall.requests, "every collection read is answered by the facade")
}

// TestBuiltinProjectsRefuseAnUnknownBearer asserts the built-in collections
// and the GET by id answer 401 to a request with no bearer or one the
// facade did not issue.
func TestBuiltinProjectsRefuseAnUnknownBearer(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, testrancher.Options{SeedToken: projectsSeed}, &recordingHandler{})
	for _, path := range []string{
		testrancher.ProjectsCollectionPath,
		testrancher.ProjectsCollectionPath + "/local",
		testrancher.LocalProjectsPath + "/p-default",
	} {
		assert.Equal(t, http.StatusUnauthorized, statusOf(t, srv.Client(), http.MethodGet, srv.URL+path, ""), path)
		assert.Equal(t, http.StatusUnauthorized, statusOf(t, srv.Client(), http.MethodGet, srv.URL+path, "other:secret"), path)
	}
}

// TestBuiltinProjectGetByID asserts the Kubernetes GET of a built-in
// project by id answers the Project object, and an unknown id the
// apiserver's 404 Status.
func TestBuiltinProjectGetByID(t *testing.T) {
	t.Parallel()
	fall := &recordingHandler{}
	_, srv := newFacade(t, testrancher.Options{SeedToken: projectsSeed}, fall)

	status, body := getDecoded(t, srv, testrancher.LocalProjectsPath+"/p-system", projectsSeed)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, "management.cattle.io/v3", body["apiVersion"])
	assert.Equal(t, "Project", body["kind"])
	assert.Equal(t, map[string]any{"name": "p-system", "namespace": "local"}, body["metadata"])
	assert.Equal(t, map[string]any{"clusterName": "local", "displayName": "System"}, body["spec"])

	status, body = getDecoded(t, srv, testrancher.LocalProjectsPath+"/p-nosuchproject", projectsSeed)
	require.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, "Status", body["kind"])
	assert.Equal(t, "v1", body["apiVersion"])
	assert.Equal(t, "Failure", body["status"])
	assert.Equal(t, "NotFound", body["reason"])
	assert.InDelta(t, 404, body["code"], 0)
	assert.Equal(t, `projects.management.cattle.io "p-nosuchproject" not found`, body["message"])
	assert.Equal(t, map[string]any{"name": "p-nosuchproject", "group": "management.cattle.io", "kind": "projects"}, body["details"])

	assert.Empty(t, fall.requests, "the GET by id is answered by the facade")
}

// TestProjectVisibilityDecidesEachCollection asserts a set ProjectVisibility
// receives the collection's cluster ("" for every cluster), that one
// cluster's collection keeps only that cluster's projects, that ok=false
// answers 401 and an error 500, and that the built-in GET by id is then not
// served.
func TestProjectVisibilityDecidesEachCollection(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var clusters []string
	fixtures := []testrancher.ProjectFixture{
		{Cluster: "c-b", ID: "p-2", DisplayName: "two"},
		{Cluster: "c-a", ID: "p-1", DisplayName: "one"},
	}
	visibility := func(_ context.Context, bearer, cluster string) ([]testrancher.ProjectFixture, bool, error) {
		mu.Lock()
		clusters = append(clusters, cluster)
		mu.Unlock()
		switch bearer {
		case "known:secret":
			return fixtures, true, nil
		case "broken:secret":
			return nil, true, errors.New("listing failed")
		default:
			return nil, false, nil
		}
	}
	fall := &recordingHandler{}
	_, srv := newFacade(t, testrancher.Options{SeedToken: projectsSeed, ProjectVisibility: visibility}, fall)

	status, body := getDecoded(t, srv, testrancher.ProjectsCollectionPath, "known:secret")
	require.Equal(t, http.StatusOK, status)
	items := projectItems(t, body)
	require.Len(t, items, 2)
	assert.Equal(t, "c-a/p-1", items[0]["id"], "sorted by cluster, then id")
	assert.Equal(t, "c-b/p-2", items[1]["id"])

	status, body = getDecoded(t, srv, testrancher.ProjectsCollectionPath+"/c-b", "known:secret")
	require.Equal(t, http.StatusOK, status)
	assert.InDelta(t, 1, body["count"], 0)
	items = projectItems(t, body)
	require.Len(t, items, 1)
	assert.Equal(t, "c-b/p-2", items[0]["id"])

	status, body = getDecoded(t, srv, testrancher.ProjectsCollectionPath+"/c-none", "known:secret")
	require.Equal(t, http.StatusOK, status)
	assert.NotContains(t, body, "count")
	assert.Empty(t, projectItems(t, body))

	assert.Equal(t, http.StatusUnauthorized, statusOf(t, srv.Client(), http.MethodGet, srv.URL+testrancher.ProjectsCollectionPath+"/c-a", "stranger:secret"))
	assert.Equal(t, http.StatusInternalServerError, statusOf(t, srv.Client(), http.MethodGet, srv.URL+testrancher.ProjectsCollectionPath+"/c-a", "broken:secret"))

	mu.Lock()
	assert.Equal(t, []string{"", "c-b", "c-none", "c-a", "c-a"}, clusters)
	mu.Unlock()

	require.Empty(t, fall.requests)
	assert.Equal(t, http.StatusTeapot, statusOf(t, srv.Client(), http.MethodGet, srv.URL+testrancher.LocalProjectsPath+"/p-default", projectsSeed),
		"with ProjectVisibility set the GET by id falls through")
	assert.Equal(t, http.StatusTeapot, statusOf(t, srv.Client(), http.MethodGet, srv.URL+testrancher.ProjectsCollectionPath+"/c-a/p-1", "known:secret"),
		"a steve GET of one project is not a collection")
}

// TestSteveCollectionCountAcrossPages asserts every page of a paged
// collection carries the whole collection's count, and an empty collection
// none.
func TestSteveCollectionCountAcrossPages(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	clusters := []testrancher.ClusterFixture{}
	_, srv := newFacade(t, testrancher.Options{
		ClustersPageSize: 1,
		ClusterVisibility: func(bearer string) ([]testrancher.ClusterFixture, bool) {
			mu.Lock()
			defer mu.Unlock()
			return clusters, bearer == "known:secret"
		},
	}, &recordingHandler{})

	status, body := getDecoded(t, srv, testrancher.ClustersCollectionPath, "known:secret")
	require.Equal(t, http.StatusOK, status)
	assert.NotContains(t, body, "count", "an empty collection carries no count")
	assert.Empty(t, projectItems(t, body))

	mu.Lock()
	clusters = []testrancher.ClusterFixture{{ID: "c-a", DisplayName: "a"}, {ID: "c-b", DisplayName: "b"}}
	mu.Unlock()
	status, body = getDecoded(t, srv, testrancher.ClustersCollectionPath, "known:secret")
	require.Equal(t, http.StatusOK, status)
	assert.InDelta(t, 2, body["count"], 0)
	assert.Len(t, projectItems(t, body), 1)
	require.Equal(t, "1", body["continue"])

	status, body = getDecoded(t, srv, testrancher.ClustersCollectionPath+"?continue=1", "known:secret")
	require.Equal(t, http.StatusOK, status)
	assert.InDelta(t, 2, body["count"], 0)
	assert.Len(t, projectItems(t, body), 1)
	assert.NotContains(t, body, "continue")
}

// namespaceItems GETs a cluster's steve namespace collection and returns its
// items by id.
func namespaceItems(t *testing.T, srv *httptest.Server, cluster string) map[string]map[string]any {
	t.Helper()
	status, body := getDecoded(t, srv, "/k8s/clusters/"+cluster+"/v1/namespaces", projectsSeed)
	require.Equal(t, http.StatusOK, status)
	items := map[string]map[string]any{}
	for _, item := range projectItems(t, body) {
		id, ok := item["id"].(string)
		require.True(t, ok, "item id is a string: %v", item)
		items[id] = item
	}
	return items
}

// itemMeta is the metadata object of a steve item.
func itemMeta(t *testing.T, items map[string]map[string]any, name string) map[string]any {
	t.Helper()
	item, ok := items[name]
	require.True(t, ok, "item %s is served", name)
	meta, ok := item["metadata"].(map[string]any)
	require.True(t, ok, "item %s has metadata", name)
	return meta
}

// TestNamespacesCollectionCarriesTheProjectAnnotation asserts a steve
// namespace item carries the project label and the <cluster>:<project>
// annotation of the cluster in the request path, the creatorId annotation,
// the resourceVersion and the phase, and that a namespace the caller has no
// get on is served as the item steve serves in that case.
func TestNamespacesCollectionCarriesTheProjectAnnotation(t *testing.T) {
	t.Parallel()
	visibility := func(string, string) ([]testrancher.NamespaceFixture, bool) {
		return []testrancher.NamespaceFixture{
			{Name: "apps", Project: "p-1"},
			{Name: "legacy", Project: "p-1", CreatorID: "u-abc", Terminating: true},
			{Name: "outside"},
			{Name: "hidden", Project: "p-1", CreatorID: "u-abc", WithoutGet: true,
				Labels:      map[string]string{"fleet.cattle.io/managed": "true", "team": "ops"},
				Annotations: map[string]string{"management.cattle.io/system-namespace": "true", "note": "x"}},
			{Name: "ending", Terminating: true, WithoutGet: true},
		}, true
	}
	_, srv := newFacade(t, testrancher.Options{SeedToken: projectsSeed, NamespaceVisibility: visibility}, &recordingHandler{})

	for _, cluster := range []string{"c-abc", "c-def"} {
		items := namespaceItems(t, srv, cluster)
		meta := func(name string) map[string]any { return itemMeta(t, items, name) }

		assert.Equal(t, "1", meta("apps")["resourceVersion"])
		assert.Equal(t, map[string]any{"kubernetes.io/metadata.name": "apps", "field.cattle.io/projectId": "p-1"}, meta("apps")["labels"])
		assert.Equal(t, map[string]any{"field.cattle.io/projectId": cluster + ":p-1"}, meta("apps")["annotations"])
		assert.Equal(t, map[string]any{"field.cattle.io/projectId": cluster + ":p-1", "field.cattle.io/creatorId": "u-abc"}, meta("legacy")["annotations"])
		assert.Equal(t, map[string]any{"phase": "Terminating"}, items["legacy"]["status"])
		assert.NotEmpty(t, meta("legacy")["deletionTimestamp"])
		assert.NotContains(t, meta("outside"), "annotations", "a namespace with no annotation serves no annotations key")

		assert.Equal(t, map[string]any{
			"name": "hidden", "resourceVersion": "1",
			"labels":      map[string]any{"fleet.cattle.io/managed": "true"},
			"annotations": map[string]any{"field.cattle.io/projectId": cluster + ":p-1", "management.cattle.io/system-namespace": "true"},
		}, meta("hidden"), "no creatorId, no other labels or annotations")
		assert.Equal(t, map[string]any{"phase": "Active"}, items["hidden"]["status"])

		assert.Equal(t, map[string]any{"name": "ending", "resourceVersion": "1"}, meta("ending"),
			"no deletionTimestamp, and no labels or annotations keys when there are none")
		assert.Equal(t, map[string]any{"phase": "Terminating"}, items["ending"]["status"])
	}
}

// TestNamespaceFixtureDedicatedFieldsWinOverTheFreeFormMaps asserts the
// project label and annotation, the creatorId annotation and the name label
// come from Project, CreatorID and Name when Labels and Annotations set the
// same keys.
func TestNamespaceFixtureDedicatedFieldsWinOverTheFreeFormMaps(t *testing.T) {
	t.Parallel()
	visibility := func(string, string) ([]testrancher.NamespaceFixture, bool) {
		return []testrancher.NamespaceFixture{{
			Name: "apps", Project: "p-1", CreatorID: "u-abc",
			Labels: map[string]string{
				"field.cattle.io/projectId":   "p-other",
				"kubernetes.io/metadata.name": "other",
			},
			Annotations: map[string]string{
				"field.cattle.io/projectId": "c-other:p-other",
				"field.cattle.io/creatorId": "u-other",
			},
		}}, true
	}
	_, srv := newFacade(t, testrancher.Options{SeedToken: projectsSeed, NamespaceVisibility: visibility}, &recordingHandler{})
	meta := itemMeta(t, namespaceItems(t, srv, "c-abc"), "apps")
	assert.Equal(t, map[string]any{"kubernetes.io/metadata.name": "apps", "field.cattle.io/projectId": "p-1"}, meta["labels"])
	assert.Equal(t, map[string]any{"field.cattle.io/projectId": "c-abc:p-1", "field.cattle.io/creatorId": "u-abc"}, meta["annotations"])
}
