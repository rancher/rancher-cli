package steve

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
)

var (
	allProjectsPath = Request{Type: ProjectsType}.Path()
	abcProjectsPath = Request{Type: ProjectsType, Namespace: "c-abc"}.Path()
)

func steveProject(cluster, project, display string) map[string]any {
	return map[string]any{"id": cluster + "/" + project, "type": "management.cattle.io.project",
		"metadata": map[string]any{"name": project},
		"spec":     map[string]any{"clusterName": cluster, "displayName": display}}
}

type getCall struct {
	gvr  schema.GroupVersionResource
	ns   string
	name string
}

type listCall struct {
	gvr      schema.GroupVersionResource
	ns       string
	selector string
}

// fakeCluster is an engine.ClusterClient that records Get and List calls and
// answers them from a script. Every other method panics through the nil
// embedded interface.
type fakeCluster struct {
	engine.ClusterClient
	mu      sync.Mutex
	gets    []getCall
	lists   []listCall
	getObj  *unstructured.Unstructured
	getErr  error
	listObj *unstructured.UnstructuredList
	listErr error
}

func (f *fakeCluster) Get(_ context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, getCall{gvr, ns, name})
	return f.getObj, f.getErr
}

func (f *fakeCluster) List(_ context.Context, gvr schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists = append(f.lists, listCall{gvr, ns, selector})
	return f.listObj, f.listErr
}

func codeOf(t *testing.T, err error) *clierr.Error {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	return ce
}

func projectsServer(t *testing.T, path string, pages map[string]stevePage) *recordingServer {
	t.Helper()
	return newServer(t, map[string]route{path: {pages: pages}})
}

func onePage(items ...map[string]any) map[string]stevePage {
	return map[string]stevePage{"": {Data: items, Count: new(len(items)), Revision: "1"}}
}

func TestProjectsAllClustersKeepsCollectionOrder(t *testing.T) {
	t.Parallel()
	srv := projectsServer(t, allProjectsPath, onePage(
		steveProject("c-m-b", "p-2", "Default"), steveProject("local", "p-l", "System"),
		steveProject("c-m-b", "p-1", "System"), steveProject("c-m-a", "p-9", "Default"),
	))
	rows, err := Projects(t.Context(), srv.getter(), "")
	require.NoError(t, err)
	assert.Equal(t, []Project{
		{Cluster: "c-m-b", ID: "p-2", DisplayName: "Default"},
		{Cluster: "local", ID: "p-l", DisplayName: "System"},
		{Cluster: "c-m-b", ID: "p-1", DisplayName: "System"},
		{Cluster: "c-m-a", ID: "p-9", DisplayName: "Default"},
	}, rows)
	assert.Equal(t, []string{"/v1/management.cattle.io.projects"}, srv.requests())
}

func TestProjectsFollowsContinueAndDeduplicates(t *testing.T) {
	t.Parallel()
	srv := projectsServer(t, allProjectsPath, map[string]stevePage{
		"":  {Data: []map[string]any{steveProject("c-m-a", "p-1", "a")}, Count: new(3), Continue: "n", Revision: "9"},
		"n": {Data: []map[string]any{steveProject("c-m-a", "p-1", "a"), steveProject("c-m-a", "p-2", "b")}, Count: new(3), Revision: "9"},
	})
	rows, err := Projects(t.Context(), srv.getter(), "")
	require.NoError(t, err)
	assert.Len(t, rows, 2, "an agreeing repeat is one row")
	assert.Equal(t, []string{allProjectsPath, allProjectsPath + "?continue=n"}, srv.requests())
}

func TestProjectsProtocolErrorsAreTerminal(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		item map[string]any
		want string
	}{
		"no id":                   {map[string]any{"type": "x", "spec": map[string]any{"clusterName": "c"}}, "item 0 has no id"},
		"not cluster/project":     {map[string]any{"id": "p-1", "spec": map[string]any{"clusterName": "c"}}, `"p-1" is not a <cluster>/<project> id`},
		"leading slash":           {map[string]any{"id": "/p-1", "spec": map[string]any{"clusterName": "c"}}, `"/p-1" is not a <cluster>/<project> id`},
		"trailing slash":          {map[string]any{"id": "c-m-a/", "spec": map[string]any{"clusterName": "c-m-a"}}, `"c-m-a/" is not a <cluster>/<project> id`},
		"extra segment":           {map[string]any{"id": "c-m-a/p-1/extra", "metadata": map[string]any{"name": "p-1/extra"}, "spec": map[string]any{"clusterName": "c-m-a"}}, `"c-m-a/p-1/extra" is not a <cluster>/<project> id`},
		"no clusterName":          {map[string]any{"id": "c-m-a/p-1", "spec": map[string]any{}}, `"c-m-a/p-1" has no spec.clusterName`},
		"clusterName disagrees":   {map[string]any{"id": "c-m-a/p-1", "spec": map[string]any{"clusterName": "c-m-b"}}, `"c-m-a/p-1" belongs to cluster "c-m-b"`},
		"metadata.name disagrees": {map[string]any{"id": "c-m-a/p-1", "metadata": map[string]any{"name": "p-2"}, "spec": map[string]any{"clusterName": "c-m-a"}}, `"c-m-a/p-1" names itself "p-2"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := projectsServer(t, allProjectsPath, onePage(tc.item))
			_, err := Projects(t.Context(), srv.getter(), "")
			require.ErrorContains(t, err, tc.want)
			assert.False(t, codeOf(t, err).Retryable, "a malformed item is never retried")
			assert.Len(t, srv.requests(), 1, "terminal on the first read")
		})
	}
}

func TestProjectsDuplicateThatDisagreesIsAChangedCollection(t *testing.T) {
	t.Parallel()
	srv := projectsServer(t, allProjectsPath, onePage(steveProject("c-m-a", "p-1", "a"), steveProject("c-m-a", "p-1", "b")))
	_, err := Projects(t.Context(), srv.getter(), "")
	require.ErrorContains(t, err, "repeated item c-m-a/p-1 disagrees with itself")
	assert.True(t, codeOf(t, err).Retryable)
	assert.Len(t, srv.requests(), 2, "one whole-read retry, then the retryable failure")
}

func TestProjectsOneClusterReadsItsNamespaceAndRefusesAnotherClustersItem(t *testing.T) {
	t.Parallel()
	srv := projectsServer(t, abcProjectsPath, onePage(steveProject("c-abc", "p-1", "web"), steveProject("c-abc", "p-2", "db")))
	rows, err := Projects(t.Context(), srv.getter(), "c-abc")
	require.NoError(t, err)
	assert.Equal(t, []Project{{Cluster: "c-abc", ID: "p-1", DisplayName: "web"}, {Cluster: "c-abc", ID: "p-2", DisplayName: "db"}}, rows)
	assert.Equal(t, []string{"/v1/management.cattle.io.projects/c-abc"}, srv.requests())

	other := projectsServer(t, abcProjectsPath, onePage(steveProject("c-abc", "p-1", "web"), steveProject("c-def", "p-2", "db")))
	_, err = Projects(t.Context(), other.getter(), "c-abc")
	require.ErrorContains(t, err, `"c-def/p-2" is not on cluster "c-abc"`)
	assert.False(t, codeOf(t, err).Retryable)
	assert.Len(t, other.requests(), 1)
}

// TestProjectsCollectionRefusals asserts a 401 keeps the root client's AUTH
// error, a 403 is RBAC naming the refused request, a 404 is a generic error
// saying the server does not serve steve as expected, a 409 is the same
// generic error and not retryable, and a 429 or a 5xx keeps the root client's
// retryable error.
func TestProjectsCollectionRefusals(t *testing.T) {
	t.Parallel()
	const path = "/v1/management.cattle.io.projects/c-abc"
	t.Run("401", func(t *testing.T) {
		t.Parallel()
		srv := newServer(t, map[string]route{path: {status: http.StatusUnauthorized}})
		_, err := Projects(t.Context(), srv.getter(), "c-abc")
		ce := codeOf(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		require.ErrorContains(t, err, "rancher root GET "+path)
		rs, ok := clierr.HTTPStatus(err)
		require.True(t, ok)
		assert.Equal(t, http.StatusUnauthorized, rs)
	})
	t.Run("403", func(t *testing.T) {
		t.Parallel()
		srv := newServer(t, map[string]route{path: {status: http.StatusForbidden}})
		_, err := Projects(t.Context(), srv.getter(), "c-abc")
		assert.Equal(t, clierr.CodeRBAC, codeOf(t, err).Code)
		require.ErrorContains(t, err, "the server refused GET "+path+" (403); Rancher answers this request with 200 for every signed-in user")
		se, ok := clierr.HTTPStatus(err)
		require.True(t, ok, "the cause stays in the chain")
		assert.Equal(t, http.StatusForbidden, se)
	})
	t.Run("Not Found", func(t *testing.T) {
		t.Parallel()
		srv := newServer(t, map[string]route{path: {status: http.StatusNotFound}})
		_, err := Projects(t.Context(), srv.getter(), "c-abc")
		assert.Equal(t, clierr.CodeGeneric, codeOf(t, err).Code)
		require.ErrorContains(t, err, "GET "+path+" answered 404; this server does not serve Rancher's steve API as expected")
	})
	t.Run("Conflict", func(t *testing.T) {
		t.Parallel()
		srv := newServer(t, map[string]route{path: {status: http.StatusConflict}})
		_, err := Projects(t.Context(), srv.getter(), "c-abc")
		ce := codeOf(t, err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.False(t, ce.Retryable)
		require.ErrorContains(t, err, "GET "+path+" answered 409; this server does not serve Rancher's steve API as expected")
	})
	t.Run("Too Many Requests", func(t *testing.T) {
		t.Parallel()
		srv := newServer(t, map[string]route{path: {status: http.StatusTooManyRequests}})
		_, err := Projects(t.Context(), srv.getter(), "c-abc")
		ce := codeOf(t, err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.True(t, ce.Retryable)
		require.ErrorContains(t, err, "rancher root GET "+path+": ")
		require.ErrorContains(t, err, " - retry later")
		assert.NotContains(t, err.Error(), "steve API")
		assert.Equal(t, []string{path}, srv.requests(), "a 429 is not read again")
	})
	t.Run("Internal Server Error", func(t *testing.T) {
		t.Parallel()
		srv := newServer(t, map[string]route{path: {status: http.StatusInternalServerError}})
		_, err := Projects(t.Context(), srv.getter(), "c-abc")
		ce := codeOf(t, err)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.True(t, ce.Retryable)
		require.ErrorContains(t, err, "rancher root GET "+path+": an error on the server (\"Internal Server Error\") has prevented the request from succeeding - retrying may succeed")
		assert.NotContains(t, err.Error(), "steve API")
	})
}

func projectFixture(t *testing.T, items ...map[string]any) (*recordingServer, Getter) {
	t.Helper()
	srv := projectsServer(t, abcProjectsPath, onePage(items...))
	return srv, srv.getter()
}

func TestResolveProjectMatchesAnIDOrADisplayName(t *testing.T) {
	t.Parallel()
	srv, get := projectFixture(t, steveProject("c-abc", "p-1", "Web"), steveProject("c-abc", "p-2", "db"))
	local := &fakeCluster{}

	p, warning, err := ResolveProject(t.Context(), get, local, "c-abc", "p-1")
	require.NoError(t, err)
	assert.Equal(t, Project{Cluster: "c-abc", ID: "p-1", DisplayName: "Web"}, p)
	assert.Empty(t, warning)

	p, warning, err = ResolveProject(t.Context(), get, local, "c-abc", "db")
	require.NoError(t, err)
	assert.Equal(t, Project{Cluster: "c-abc", ID: "p-2", DisplayName: "db"}, p)
	assert.Empty(t, warning)

	assert.Empty(t, local.gets, "a match never falls back to a GET")
	assert.Equal(t, []string{abcProjectsPath, abcProjectsPath}, srv.requests())
}

func TestResolveProjectIDWinsOverAnotherProjectsDisplayName(t *testing.T) {
	t.Parallel()
	_, get := projectFixture(t, steveProject("c-abc", "p-1", "first"), steveProject("c-abc", "p-2", "p-1"))
	p, warning, err := ResolveProject(t.Context(), get, &fakeCluster{}, "c-abc", "p-1")
	require.NoError(t, err)
	assert.Equal(t, Project{Cluster: "c-abc", ID: "p-1", DisplayName: "first"}, p)
	assert.Equal(t, "--project 'p-1' is the id of project 'p-1' and the display name of project 'p-2'; the command uses the one with that id. To use the other one, pass --project 'p-2'", warning)

	_, get = projectFixture(t, steveProject("c-abc", "p-1", "first"), steveProject("c-abc", "p-3", "p-1"), steveProject("c-abc", "p-2", "p-1"))
	p, warning, err = ResolveProject(t.Context(), get, &fakeCluster{}, "c-abc", "p-1")
	require.NoError(t, err)
	assert.Equal(t, "p-1", p.ID)
	assert.Equal(t, "--project 'p-1' is the id of project 'p-1' and the display name of projects 'p-2', 'p-3'; the command uses the one with that id. To use one of the others, pass --project with its id", warning)
}

func TestResolveProjectDuplicateDisplayNameIsAmbiguous(t *testing.T) {
	t.Parallel()
	_, get := projectFixture(t, steveProject("c-abc", "p-3", "web"), steveProject("c-abc", "p-9", "other"), steveProject("c-abc", "p-1", "web"))
	local := &fakeCluster{}
	_, _, err := ResolveProject(t.Context(), get, local, "c-abc", "web")
	ce := codeOf(t, err)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Equal(t, "2 projects have display name 'web'; pass the project id", ce.Message)
	assert.Equal(t, []clierr.Candidate{{ID: "p-1", Name: "web"}, {ID: "p-3", Name: "web"}}, ce.Candidates)
	assert.Empty(t, local.gets)
}

func TestResolveProjectNotFoundNamesEveryCandidate(t *testing.T) {
	t.Parallel()
	_, get := projectFixture(t, steveProject("c-abc", "p-2", "db"), steveProject("c-abc", "p-1", "api"))
	local := &fakeCluster{getErr: clierr.New(clierr.CodeNotFound, "gone")}
	_, _, err := ResolveProject(t.Context(), get, local, "c-abc", "web")
	ce := codeOf(t, err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "project not found: 'web'; it is not among the projects you can read on cluster 'c-abc'; pass a project id or display name", ce.Message)
	assert.Equal(t, clierr.Rancher+" access projects --cluster 'c-abc'", ce.Hint)
	assert.Equal(t, []clierr.Candidate{{ID: "p-2", Name: "db"}, {ID: "p-1", Name: "api"}}, ce.Candidates)
}

func TestResolveProjectEmptyCollectionSaysYouCanReadNone(t *testing.T) {
	t.Parallel()
	_, get := projectFixture(t)
	local := &fakeCluster{getErr: clierr.New(clierr.CodeNotFound, "gone")}
	_, _, err := ResolveProject(t.Context(), get, local, "c-abc", "web")
	ce := codeOf(t, err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "project not found: 'web'; you can read no project on cluster 'c-abc'; a cluster owner or a Rancher administrator grants project roles", ce.Message)
	assert.Empty(t, ce.Hint)
	assert.Empty(t, ce.Candidates)
}

func TestResolveProjectFallsBackToOneGetByID(t *testing.T) {
	t.Parallel()
	_, get := projectFixture(t, steveProject("c-abc", "p-2", "db"))
	obj := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "p-new"},
		"spec":     map[string]any{"displayName": "Brand new"},
	}}
	local := &fakeCluster{getObj: obj}
	p, warning, err := ResolveProject(t.Context(), get, local, "c-abc", "p-new")
	require.NoError(t, err)
	assert.Equal(t, Project{Cluster: "c-abc", ID: "p-new", DisplayName: "Brand new"}, p)
	assert.Empty(t, warning)
	assert.Equal(t, []getCall{{
		gvr: schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "projects"},
		ns:  "c-abc", name: "p-new",
	}}, local.gets)
}

// TestResolveProjectGetFallbackErrors asserts a refused or missing GET
// keeps the NOT_FOUND the listing produced, and any other GET failure is
// returned as it is.
func TestResolveProjectGetFallbackErrors(t *testing.T) {
	t.Parallel()
	wantMsg := "project not found: 'web'; it is not among the projects you can read on cluster 'c-abc'; pass a project id or display name"
	for name, code := range map[string]string{"forbidden": clierr.CodeRBAC, "not found": clierr.CodeNotFound} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, get := projectFixture(t, steveProject("c-abc", "p-2", "db"))
			local := &fakeCluster{getErr: clierr.New(code, "from the GET")}
			_, _, err := ResolveProject(t.Context(), get, local, "c-abc", "web")
			ce := codeOf(t, err)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			assert.Equal(t, wantMsg, ce.Message)
			assert.Len(t, local.gets, 1)
		})
	}

	t.Run("other failure", func(t *testing.T) {
		t.Parallel()
		_, get := projectFixture(t, steveProject("c-abc", "p-2", "db"))
		boom := clierr.New(clierr.CodeGeneric, "the proxy failed")
		local := &fakeCluster{getErr: boom}
		_, _, err := ResolveProject(t.Context(), get, local, "c-abc", "web")
		assert.Same(t, boom, err)
	})
}

// TestResolveProjectNoGetForAValueThatIsNotAName asserts a display name
// with a space and a <cluster>/<project> value, neither of which can be a
// project name, are NOT_FOUND without a GET.
func TestResolveProjectNoGetForAValueThatIsNotAName(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"my web", "c-abc/p-1"} {
		t.Run(v, func(t *testing.T) {
			t.Parallel()
			_, get := projectFixture(t, steveProject("c-abc", "p-2", "db"))
			local := &fakeCluster{getObj: &unstructured.Unstructured{Object: map[string]any{}}}
			_, _, err := ResolveProject(t.Context(), get, local, "c-abc", v)
			assert.Equal(t, clierr.CodeNotFound, codeOf(t, err).Code)
			assert.Empty(t, local.gets)
		})
	}
}

func TestResolveProjectListingErrorIsReturned(t *testing.T) {
	t.Parallel()
	for status, code := range map[int]string{http.StatusForbidden: clierr.CodeRBAC, http.StatusInternalServerError: clierr.CodeGeneric} {
		srv := newServer(t, map[string]route{abcProjectsPath: {status: status}})
		local := &fakeCluster{}
		_, _, err := ResolveProject(t.Context(), srv.getter(), local, "c-abc", "web")
		assert.Equal(t, code, codeOf(t, err).Code, "status %d", status)
		assert.Empty(t, local.gets, "status %d: a refused listing sends no GET by id", status)
	}
}

func namespaceItem(name string, labels, annotations map[string]any) map[string]any {
	meta := map[string]any{"name": name}
	if labels != nil {
		meta["labels"] = labels
	}
	if annotations != nil {
		meta["annotations"] = annotations
	}
	return map[string]any{"id": name, "type": "namespace", "metadata": meta}
}

func namespacesServer(t *testing.T, items ...map[string]any) *recordingServer {
	t.Helper()
	return projectsServer(t, "/k8s/clusters/c-abc/v1/namespaces", onePage(items...))
}

// annotatedNamespace is a namespace object whose project annotation is value;
// an empty value leaves the annotation off.
func annotatedNamespace(name, value string, labels map[string]any) unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if value != "" {
		meta["annotations"] = map[string]any{"field.cattle.io/projectId": value}
	}
	if labels != nil {
		meta["labels"] = labels
	}
	return unstructured.Unstructured{Object: map[string]any{"metadata": meta}}
}

// TestProjectNamespacesUsesTheFilteredList asserts the namespace LIST is
// narrowed by the project label and kept by the annotation: a namespace
// with the label and no annotation (Rancher no longer counts it in the
// project) is not a member, one with the annotation only is, and one
// whose annotation names another cluster is not.
func TestProjectNamespacesUsesTheFilteredList(t *testing.T) {
	t.Parallel()
	list := &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		annotatedNamespace("ns-a", "c-abc:p-1", map[string]any{"field.cattle.io/projectId": "p-1"}),
		annotatedNamespace("ns-b", "c-abc:p-1", nil),
		annotatedNamespace("with-project-label", "", map[string]any{"field.cattle.io/projectId": "p-1"}),
		annotatedNamespace("other-cluster", "c-def:p-1", map[string]any{"field.cattle.io/projectId": "p-1"}),
	}}
	down := &fakeCluster{listObj: list}
	srv := namespacesServer(t)
	got, err := ProjectNamespaces(t.Context(), down, srv.getter(), "c-abc", "p-1")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"ns-a": true, "ns-b": true}, got)
	assert.Equal(t, []listCall{{
		gvr:      schema.GroupVersionResource{Version: "v1", Resource: "namespaces"},
		selector: "field.cattle.io/projectId=p-1",
	}}, down.lists)
	assert.Empty(t, srv.requests(), "the LIST succeeded, steve is not read")
}

// TestProjectNamespacesEmptyFilteredListIsAnEmptySet asserts a project
// with no namespace is an empty set, not an error, and steve is not read.
func TestProjectNamespacesEmptyFilteredListIsAnEmptySet(t *testing.T) {
	t.Parallel()
	down := &fakeCluster{listObj: &unstructured.UnstructuredList{}}
	srv := namespacesServer(t)
	got, err := ProjectNamespaces(t.Context(), down, srv.getter(), "c-abc", "p-1")
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.NotNil(t, got)
	assert.Empty(t, srv.requests())
}

func TestProjectNamespacesRefusedListReadsSteveAndKeepsTheProjectsNamespaces(t *testing.T) {
	t.Parallel()
	down := &fakeCluster{listErr: clierr.New(clierr.CodeRBAC, "forbidden")}
	srv := namespacesServer(t,
		namespaceItem("by-label", map[string]any{"field.cattle.io/projectId": "p-1"}, nil),
		namespaceItem("by-annotation", nil, map[string]any{"field.cattle.io/projectId": "c-abc:p-1"}),
		namespaceItem("other-label", map[string]any{"field.cattle.io/projectId": "p-2"}, nil),
		namespaceItem("other-annotation", nil, map[string]any{"field.cattle.io/projectId": "c-abc:p-2"}),
		namespaceItem("other-cluster", nil, map[string]any{"field.cattle.io/projectId": "c-def:p-1"}),
		namespaceItem("no-project", nil, nil),
	)
	got, err := ProjectNamespaces(t.Context(), down, srv.getter(), "c-abc", "p-1")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"by-annotation": true}, got, "the label alone does not make a namespace a member")
	assert.Equal(t, []string{"/k8s/clusters/c-abc/v1/namespaces"}, srv.requests())
}

// TestProjectNamespaceObjectsItemsWithoutGetAreMembersByAnnotation asserts an
// item steve returns for a namespace the caller cannot get (the annotation
// and no labels) is a member by its annotation and is not readable in full,
// while a full object is.
func TestProjectNamespaceObjectsItemsWithoutGetAreMembersByAnnotation(t *testing.T) {
	t.Parallel()
	down := &fakeCluster{listErr: clierr.New(clierr.CodeRBAC, "forbidden")}
	srv := namespacesServer(t,
		namespaceItem("without-get", map[string]any{}, map[string]any{"field.cattle.io/projectId": "c-abc:p-1"}),
		namespaceItem("full", map[string]any{"kubernetes.io/metadata.name": "full"}, map[string]any{"field.cattle.io/projectId": "c-abc:p-1"}),
	)
	items, viaSteve, err := ProjectNamespaceObjects(t.Context(), down, srv.getter(), "c-abc", "p-1")
	require.NoError(t, err)
	assert.True(t, viaSteve)
	require.Len(t, items, 2)
	byName := map[string]bool{}
	for i := range items {
		byName[items[i].GetName()] = ReadableInFull(&items[i])
	}
	assert.Equal(t, map[string]bool{"without-get": false, "full": true}, byName)
}

// TestProjectNamespaceObjectsListIsNotViaSteve asserts a successful LIST
// reports its items as not from steve.
func TestProjectNamespaceObjectsListIsNotViaSteve(t *testing.T) {
	t.Parallel()
	down := &fakeCluster{listObj: &unstructured.UnstructuredList{Items: []unstructured.Unstructured{annotatedNamespace("ns-a", "c-abc:p-1", nil)}}}
	items, viaSteve, err := ProjectNamespaceObjects(t.Context(), down, namespacesServer(t).getter(), "c-abc", "p-1")
	require.NoError(t, err)
	assert.False(t, viaSteve)
	require.Len(t, items, 1)
	assert.Equal(t, "ns-a", items[0].GetName())
}

func TestProjectNamespacesSteve401IsRBACWithAHint(t *testing.T) {
	t.Parallel()
	down := &fakeCluster{listErr: clierr.New(clierr.CodeRBAC, "forbidden")}
	srv := newServer(t, map[string]route{"/k8s/clusters/c-abc/v1/namespaces": {status: http.StatusUnauthorized}})
	_, err := ProjectNamespaces(t.Context(), down, srv.getter(), "c-abc", "p-1")
	ce := codeOf(t, err)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, "Rancher refused access to cluster 'c-abc' (401 with no body, its answer to a user with no role on that cluster)", ce.Message)
	assert.Equal(t, clierr.Rancher+" access clusters", ce.Hint)
}

func TestProjectNamespacesListErrorThatIsNotRBACIsReturnedAsIs(t *testing.T) {
	t.Parallel()
	boom := clierr.New(clierr.CodeGeneric, "the proxy failed")
	down := &fakeCluster{listErr: boom}
	srv := namespacesServer(t)
	_, err := ProjectNamespaces(t.Context(), down, srv.getter(), "c-abc", "p-1")
	assert.Same(t, boom, err)
	assert.Empty(t, srv.requests(), "no steve read")
}

func TestProjectNamespacesMalformedProjectIDIsValidationBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	down := &fakeCluster{}
	srv := namespacesServer(t)
	_, err := ProjectNamespaces(t.Context(), down, srv.getter(), "c-abc", "bad id!")
	assert.Equal(t, clierr.CodeValidation, codeOf(t, err).Code)
	assert.Empty(t, down.lists)
	assert.Empty(t, srv.requests())
}
