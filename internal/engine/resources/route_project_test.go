package resources

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// routeProjectClient is a List-only ClusterClient fake keyed by GVR resource
// name, so a single fake serves both the management "projects" list
// (ResolveProject) and the downstream "namespaces" list (ProjectNamespaces)
// with independent seeded data. It records every List call for assertion.
type routeProjectClient struct {
	lists map[string]*unstructured.UnstructuredList

	listCalls []routeListCall
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

func projectItem(id, displayName string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": id},
		"spec":     map[string]any{"displayName": displayName},
	}}
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

// newRouteProjectFixture seeds a project "p-1" and a single member namespace
// "ns-a"; the fake namespace list already represents what a real server
// would return after applying the membership label selector.
func newRouteProjectFixture() (*routeProjectClient, *recFactory) {
	client := &routeProjectClient{lists: map[string]*unstructured.UnstructuredList{
		"projects":   {Items: []unstructured.Unstructured{projectItem("p-1", "web")}},
		"namespaces": {Items: []unstructured.Unstructured{namespaceItem("ns-a")}},
	}}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	return client, fac
}

func TestRouteDownstreamNSPlainNamedOpMemberNamespacePasses(t *testing.T) {
	t.Parallel()
	_, fac := newRouteProjectFixture()
	clusterID, ns, effCluster, scope, err := Route(context.Background(), fac, podKindDownstream(),
		input{s: map[string]string{"cluster": "prod", "namespace": "ns-a", "project": "p-1"}}, true)
	require.NoError(t, err)
	assert.Equal(t, "c-abc", clusterID)
	assert.Equal(t, "ns-a", ns)
	assert.Equal(t, "c-abc", effCluster)
	require.NotNil(t, scope)
	assert.NotNil(t, scope.Members)
}

func TestRouteDownstreamNSPlainNamedOpNonMemberNamespaceRejected(t *testing.T) {
	t.Parallel()
	_, fac := newRouteProjectFixture()
	_, _, _, _, err := Route(context.Background(), fac, podKindDownstream(),
		input{s: map[string]string{"cluster": "prod", "namespace": "ns-z", "project": "p-1"}}, true)
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "ns-z")
	assert.Contains(t, ce.Message, "p-1")
}

func TestRouteNamespacePlusProjectWithMemberNamespacePasses(t *testing.T) {
	t.Parallel()
	_, fac := newRouteProjectFixture()
	_, ns, _, scope, err := Route(context.Background(), fac, podKindDownstream(),
		input{s: map[string]string{"cluster": "prod", "namespace": "ns-a", "project": "p-1"}}, false)
	require.NoError(t, err)
	assert.Equal(t, "ns-a", ns)
	require.NotNil(t, scope)
}

func TestRouteDownstreamNSPlainListWithoutNamespaceCarriesMemberSet(t *testing.T) {
	t.Parallel()
	_, fac := newRouteProjectFixture()
	_, ns, _, scope, err := Route(context.Background(), fac, podKindDownstream(),
		input{s: map[string]string{"cluster": "prod", "project": "p-1"}}, false)
	require.NoError(t, err)
	assert.Equal(t, "", ns)
	require.NotNil(t, scope)
	assert.NotNil(t, scope.Members)
	assert.Equal(t, map[string]bool{"ns-a": true}, scope.Members)
}

func TestRouteContextProjectFallsBackWhenFlagEmpty(t *testing.T) {
	t.Parallel()
	_, fac := newRouteProjectFixture()
	fac.ctxProject = "p-1"
	_, _, _, scope, err := Route(context.Background(), fac, podKindDownstream(),
		input{s: map[string]string{"cluster": "prod"}}, false)
	require.NoError(t, err)
	require.NotNil(t, scope)
	assert.NotNil(t, scope.Members)
}

func TestRouteFlagProjectTakesPrecedenceOverContext(t *testing.T) {
	t.Parallel()
	client := &routeProjectClient{lists: map[string]*unstructured.UnstructuredList{
		"projects":   {Items: []unstructured.Unstructured{projectItem("p-1", "web"), projectItem("p-2", "db")}},
		"namespaces": {Items: []unstructured.Unstructured{namespaceItem("ns-a")}},
	}}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}, ctxProject: "p-2"}
	_, _, _, scope, err := Route(context.Background(), fac, podKindDownstream(),
		input{s: map[string]string{"cluster": "prod", "project": "p-1"}}, false)
	require.NoError(t, err)
	require.NotNil(t, scope)
	require.NotNil(t, scope.Members)
	// Precedence is observable in the member-set fetch: the namespaces list
	// must be scoped by the FLAG's project (p-1), not the context's (p-2).
	var nsSelectors []string
	for _, call := range client.listCalls {
		if call.resource == "namespaces" {
			nsSelectors = append(nsSelectors, call.selector)
		}
	}
	assert.Equal(t, []string{"field.cattle.io/projectId=p-1"}, nsSelectors,
		"the --project flag wins over the context project")
}

func TestRouteNamespaceKindWithProjectIsNotRejected(t *testing.T) {
	t.Parallel()
	_, fac := newRouteProjectFixture()
	clusterID, ns, _, scope, err := Route(context.Background(), fac, namespaceKindDownstream(),
		input{s: map[string]string{"cluster": "prod", "project": "p-1"}}, false)
	require.NoError(t, err)
	assert.Equal(t, "c-abc", clusterID)
	assert.Equal(t, "", ns)
	require.NotNil(t, scope, "the namespace kind + project is a label-selector special case, not a rejection")
	assert.Equal(t, "field.cattle.io/projectId=p-1", scope.Selector)
}

func TestGetNamespacesWithProjectListsUsingLabelSelector(t *testing.T) {
	t.Parallel()
	client := &routeProjectClient{lists: map[string]*unstructured.UnstructuredList{
		"projects":   {Items: []unstructured.Unstructured{projectItem("p-1", "web")}},
		"namespaces": {Items: []unstructured.Unstructured{namespaceItem("ns-a")}},
	}}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	nsKind, ok := kinds.Lookup("namespace")
	require.True(t, ok)

	res := execGet(t, NewGet(nsKind, fac), input{s: map[string]string{"cluster": "prod", "project": "p-1"}})
	assert.Equal(t, "NamespaceList", res.Kind)

	var sawSelector bool
	for _, call := range client.listCalls {
		if call.resource == "namespaces" && call.selector == "field.cattle.io/projectId=p-1" {
			sawSelector = true
		}
	}
	assert.True(t, sawSelector, "expected a namespaces List call carrying the project label selector, got %+v", client.listCalls)
}

var _ engine.ClusterClient = (*routeProjectClient)(nil)
