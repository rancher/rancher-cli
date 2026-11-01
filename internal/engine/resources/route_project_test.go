package resources

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// routeProjectClient is a List-only ClusterClient fake keyed by GVR resource
// name. It records every List call, so a test can tell the requests Route
// sends from the ones the command sends after it.
type routeProjectClient struct {
	lists map[string]*unstructured.UnstructuredList

	listCalls []routeListCall

	// tables is what ListTable answers by resource name; the requests it
	// receives are recorded like List's.
	tables     map[string]engine.TableAnswer
	tableCalls []routeListCall
}

var _ engine.TableReader = (*routeProjectClient)(nil)

func (c *routeProjectClient) ListTable(_ context.Context, gvr schema.GroupVersionResource, ns, selector string) (engine.TableAnswer, error) {
	c.tableCalls = append(c.tableCalls, routeListCall{resource: gvr.Resource, ns: ns, selector: selector})
	return c.tables[gvr.Resource], nil
}

func (c *routeProjectClient) GetTable(context.Context, schema.GroupVersionResource, string, string) (engine.TableAnswer, error) {
	return engine.TableAnswer{}, errors.New("GetTable not scripted in this fake")
}

type routeListCall struct {
	resource string
	ns       string
	selector string
}

func (c *routeProjectClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (c *routeProjectClient) List(_ context.Context, gvr schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error) {
	c.listCalls = append(c.listCalls, routeListCall{resource: gvr.Resource, ns: ns, selector: selector})
	if l, ok := c.lists[gvr.Resource]; ok {
		return l, nil
	}
	return &unstructured.UnstructuredList{}, nil
}

func (c *routeProjectClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (c *routeProjectClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (c *routeProjectClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}
func (c *routeProjectClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}

func namespaceItem(name string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name}}}
}

func podKindDownstream() kinds.Kind {
	return kinds.Kind{Name: "pod", Scope: kinds.Downstream, Policy: kinds.NSPlain}
}

func namespaceKindDownstream() kinds.Kind {
	return kinds.Kind{Name: "namespace", Scope: kinds.Downstream, Policy: kinds.NSNone}
}

// newRouteProjectFixture serves one namespace "ns-a" on cluster "prod"
// (id c-abc); the namespace list stands for what a server returns after
// applying a label selector.
func newRouteProjectFixture() (*routeProjectClient, *recFactory) {
	client := &routeProjectClient{lists: map[string]*unstructured.UnstructuredList{
		"namespaces": {Items: []unstructured.Unstructured{namespaceItem("ns-a")}},
	}}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	return client, fac
}

// projectContext is the context the pipeline gives a command whose typed
// --project resolved to project p-1 ("web") on cluster c-abc, with the
// member namespaces given. The returned counter counts the member reads.
func projectContext(t *testing.T, members ...string) (context.Context, *atomic.Int32) {
	t.Helper()
	reads := &atomic.Int32{}
	set := map[string]bool{}
	for _, m := range members {
		set[m] = true
	}
	ctx := runtime.WithProject(t.Context(), runtime.Project{
		Cluster: "c-abc", ID: "p-1", DisplayName: "web",
		Namespaces: func(context.Context) (map[string]bool, error) {
			reads.Add(1)
			return set, nil
		},
	})
	return ctx, reads
}

// unreadProjectContext is a context whose project lookup fails the test
// when called: a command that refuses before reading the project never
// calls it.
func unreadProjectContext(t *testing.T) context.Context {
	t.Helper()
	return runtime.WithProjectLookup(t.Context(), func(context.Context) (runtime.Project, error) {
		t.Error("the project was read")
		return runtime.Project{}, errors.New("unexpected project read")
	})
}

func TestRouteDownstreamNSPlainNamedOpMemberNamespacePasses(t *testing.T) {
	t.Parallel()
	client, fac := newRouteProjectFixture()
	ctx, reads := projectContext(t, "ns-a")
	clusterID, ns, effCluster, scope, err := Route(ctx, fac, podKindDownstream(),
		input{s: map[string]string{"cluster": "prod", "namespace": "ns-a", "project": "web"}}, OpNamed, "web-0")
	require.NoError(t, err)
	assert.Equal(t, "c-abc", clusterID)
	assert.Equal(t, "ns-a", ns)
	assert.Equal(t, "c-abc", effCluster)
	require.NotNil(t, scope)
	assert.Equal(t, map[string]bool{"ns-a": true}, scope.Members)
	assert.Equal(t, int32(1), reads.Load(), "the member set is the project's own read")
	assert.Empty(t, client.listCalls, "Route sends no request of its own for the project")
}

func TestRouteDownstreamNSPlainNamedOpNonMemberNamespaceRejected(t *testing.T) {
	t.Parallel()
	_, fac := newRouteProjectFixture()
	ctx, _ := projectContext(t, "ns-a")
	_, _, _, _, err := Route(ctx, fac, podKindDownstream(),
		input{s: map[string]string{"cluster": "prod", "namespace": "ns-z", "project": "web"}}, OpNamed, "web-0")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "ns-z")
	assert.Contains(t, ce.Message, "p-1", "the refusal names the resolved project id")
}

func TestRouteNamespacePlusProjectWithMemberNamespacePasses(t *testing.T) {
	t.Parallel()
	_, fac := newRouteProjectFixture()
	ctx, _ := projectContext(t, "ns-a")
	_, ns, _, scope, err := Route(ctx, fac, podKindDownstream(),
		input{s: map[string]string{"cluster": "prod", "namespace": "ns-a", "project": "web"}}, OpList, "")
	require.NoError(t, err)
	assert.Equal(t, "ns-a", ns)
	require.NotNil(t, scope)
}

func TestRouteDownstreamNSPlainListWithoutNamespaceCarriesMemberSet(t *testing.T) {
	t.Parallel()
	client, fac := newRouteProjectFixture()
	ctx, _ := projectContext(t, "ns-a", "ns-b")
	_, ns, _, scope, err := Route(ctx, fac, podKindDownstream(),
		input{s: map[string]string{"cluster": "prod", "project": "web"}}, OpList, "")
	require.NoError(t, err)
	assert.Equal(t, "", ns)
	require.NotNil(t, scope)
	assert.Equal(t, map[string]bool{"ns-a": true, "ns-b": true}, scope.Members)
	assert.Empty(t, scope.Selector)
	assert.Empty(t, client.listCalls, "Route sends no request of its own for the project")
}

// TestRouteReadsTheProjectOnTheClientCluster asserts Route reads the
// project with the cluster it resolved from --cluster: a project resolved
// on another cluster is refused as an internal error, never used.
func TestRouteReadsTheProjectOnTheClientCluster(t *testing.T) {
	t.Parallel()
	_, fac := newRouteProjectFixture()
	ctx := runtime.WithProject(t.Context(), runtime.Project{Cluster: "c-other", ID: "p-1",
		Namespaces: func(context.Context) (map[string]bool, error) { return map[string]bool{"ns-a": true}, nil }})
	for _, tc := range []struct {
		name string
		kind kinds.Kind
	}{
		{"namespaced kind", podKindDownstream()},
		{"namespace kind", namespaceKindDownstream()},
	} {
		_, _, _, _, err := Route(ctx, fac, tc.kind,
			input{s: map[string]string{"cluster": "prod", "project": "web"}}, OpList, "")
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce, tc.name)
		assert.Equal(t, clierr.CodeGeneric, ce.Code, tc.name)
		require.ErrorContains(t, err, "c-abc", tc.name)
	}
}

func TestRouteNamespaceKindListWithProjectIsALabelSelector(t *testing.T) {
	t.Parallel()
	client, fac := newRouteProjectFixture()
	ctx, reads := projectContext(t, "ns-a")
	clusterID, ns, effCluster, scope, err := Route(ctx, fac, namespaceKindDownstream(),
		input{s: map[string]string{"cluster": "prod", "project": "web"}}, OpList, "")
	require.NoError(t, err)
	assert.Equal(t, "c-abc", clusterID)
	assert.Equal(t, "", ns)
	assert.Equal(t, "c-abc", effCluster)
	require.NotNil(t, scope, "the namespace kind under --project lists by label selector")
	assert.Equal(t, "field.cattle.io/projectId=p-1", scope.Selector, "the selector carries the resolved project id")
	assert.Nil(t, scope.Members)
	assert.Zero(t, reads.Load(), "a list reads no member set: the selector does the filtering")
	assert.Empty(t, client.listCalls)
}

func TestRouteNamespaceKindNamedChecksMembership(t *testing.T) {
	t.Parallel()
	_, fac := newRouteProjectFixture()
	ctx, reads := projectContext(t, "ns-a")
	in := input{s: map[string]string{"cluster": "prod", "project": "web"}}

	clusterID, _, _, scope, err := Route(ctx, fac, namespaceKindDownstream(), in, OpNamed, "ns-a")
	require.NoError(t, err)
	assert.Equal(t, "c-abc", clusterID)
	assert.Nil(t, scope, "a named namespace is checked, not filtered")
	assert.Equal(t, int32(1), reads.Load())

	_, _, _, _, err = Route(ctx, fac, namespaceKindDownstream(), in, OpNamed, "ns-z")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "ns-z")
	assert.Contains(t, ce.Message, "p-1")
}

// TestRouteNamedBranchReadsTheNameArgument asserts the namespace kind's
// membership check uses the name the caller passes, never the input's
// positional arguments: a command whose positional is not the object name
// (create resource has none) must still be checked against the right
// namespace.
func TestRouteNamedBranchReadsTheNameArgument(t *testing.T) {
	t.Parallel()
	_, fac := newRouteProjectFixture()
	ctx, _ := projectContext(t, "ns-a")

	_, _, _, _, err := Route(ctx, fac, namespaceKindDownstream(),
		input{args: []string{"ns-z"}, s: map[string]string{"cluster": "prod", "project": "web"}}, OpNamed, "ns-a")
	require.NoError(t, err, "the member name passed to Route is checked, not the non-member positional")

	_, _, _, _, err = Route(ctx, fac, namespaceKindDownstream(),
		input{args: []string{"ns-a"}, s: map[string]string{"cluster": "prod", "project": "web"}}, OpNamed, "ns-z")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce, "the non-member name passed to Route is refused despite the member positional")
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "ns-z")
}

// TestRouteCreateNamespaceWithProjectIsRefused asserts creating a namespace
// with --project is refused before the project is read or a cluster is
// resolved, with a hint naming create namespace only when the name and
// --cluster were both typed: the hint never names a value nobody typed.
func TestRouteCreateNamespaceWithProjectIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		objName  string
		cluster  string
		wantHint string
	}{
		{"name and cluster", "ns1", "prod", clierr.Rancher + " create namespace 'ns1' --cluster 'prod' --project 'web' --dry-run"},
		{"no name", "", "prod", ""},
		{"no cluster", "ns1", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, fac := newRouteProjectFixture()
			flags := map[string]string{"project": "web"}
			if tc.cluster != "" {
				flags["cluster"] = tc.cluster
			}
			_, _, _, _, err := Route(unreadProjectContext(t), fac, namespaceKindDownstream(), input{s: flags}, OpCreate, tc.objName)
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, "--project does not place a namespace created from a manifest in a project; <name> is the namespace, <cluster> its cluster and <project> the project; add --apply after reading the preview: "+clierr.Rancher+" create namespace <name> --cluster <cluster> --project <project> --dry-run", ce.Message)
			assert.Equal(t, tc.wantHint, ce.Hint)
			assert.Empty(t, fac.clusterCalls)
			assert.Empty(t, client.listCalls)
		})
	}
}

// TestRouteCreateNamespacedKindWithProjectChecksMembership asserts a
// create of a project scopable namespaced kind takes --project: its
// namespace must be in this project.
func TestRouteCreateNamespacedKindWithProjectChecksMembership(t *testing.T) {
	t.Parallel()
	_, fac := newRouteProjectFixture()
	ctx, _ := projectContext(t, "ns-a")
	_, ns, _, scope, err := Route(ctx, fac, podKindDownstream(),
		input{s: map[string]string{"cluster": "prod", "namespace": "ns-a", "project": "web"}}, OpCreate, "web-0")
	require.NoError(t, err)
	assert.Equal(t, "ns-a", ns)
	require.NotNil(t, scope)

	_, _, _, _, err = Route(ctx, fac, podKindDownstream(),
		input{s: map[string]string{"cluster": "prod", "namespace": "ns-z", "project": "web"}}, OpCreate, "web-0")
	requireValidation(t, err)
}

// TestRouteWithoutProjectReadsNoProject asserts an invocation with no
// --project typed carries no project scope, for both project scopable
// families.
func TestRouteWithoutProjectReadsNoProject(t *testing.T) {
	t.Parallel()
	_, fac := newRouteProjectFixture()
	for _, k := range []kinds.Kind{podKindDownstream(), namespaceKindDownstream()} {
		_, _, _, scope, err := Route(t.Context(), fac, k, input{s: map[string]string{"cluster": "prod"}}, OpList, "")
		require.NoError(t, err, k.Name)
		assert.Nil(t, scope, k.Name)
	}
}

func TestGetNamespacesWithProjectListsUsingLabelSelector(t *testing.T) {
	t.Parallel()
	client, fac := newRouteProjectFixture()
	nsKind, ok := kinds.Lookup("namespace")
	require.True(t, ok)
	ctx, _ := projectContext(t, "ns-a")

	res, err := NewGet(nsKind, fac).(runtime.ReadCommand).Execute(ctx, input{s: map[string]string{"cluster": "prod", "project": "web"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, "NamespaceList", res.Kind)
	assert.Equal(t, []routeListCall{{resource: "namespaces", selector: "field.cattle.io/projectId=p-1"}}, client.listCalls)
}

// annotatedNamespaceItem is a namespace whose labels and annotations are as
// given; an empty value leaves that key off.
func annotatedNamespaceItem(name, label, annotation string) unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if label != "" {
		meta["labels"] = map[string]any{"field.cattle.io/projectId": label}
	}
	if annotation != "" {
		meta["annotations"] = map[string]any{"field.cattle.io/projectId": annotation}
	}
	return unstructured.Unstructured{Object: map[string]any{"metadata": meta}}
}

// TestGetNamespacesWithProjectKeepsTheNamespacesTheAnnotationNames asserts
// the label selector only narrows the list: the rows are the namespaces
// whose annotation names the project on the cluster. A namespace with the
// label and no annotation, and one annotated for another cluster, are
// dropped; one with the annotation only is kept.
func TestGetNamespacesWithProjectKeepsTheNamespacesTheAnnotationNames(t *testing.T) {
	t.Parallel()
	client, fac := newRouteProjectFixture()
	client.lists["namespaces"] = &unstructured.UnstructuredList{Items: []unstructured.Unstructured{
		annotatedNamespaceItem("both", "p-1", "c-abc:p-1"),
		annotatedNamespaceItem("annotation-only", "", "c-abc:p-1"),
		annotatedNamespaceItem("with-project-label", "p-1", ""),
		annotatedNamespaceItem("other-cluster", "p-1", "c-def:p-1"),
		annotatedNamespaceItem("other-project", "p-1", "c-abc:p-2"),
	}}
	ctx, _ := projectContext(t)
	res, err := NewGet(mustKind(t, "namespace"), fac).(runtime.ReadCommand).Execute(ctx,
		input{s: map[string]string{"cluster": "prod", "project": "web"}}, nil)
	require.NoError(t, err)
	tbl, ok := res.Data.(table)
	require.True(t, ok)
	var names []string
	for _, item := range tbl.items {
		names = append(names, item.GetName())
	}
	assert.Equal(t, []string{"both", "annotation-only"}, names)
}

var _ engine.ClusterClient = (*routeProjectClient)(nil)

func (*routeProjectClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*routeProjectClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}
