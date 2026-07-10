package projects

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// listClient is a minimal engine.ClusterClient fake that only supports List,
// recording the ns/selector it was called with and returning a seeded list
// keyed by GVR resource.
type listClient struct {
	lists map[string]*unstructured.UnstructuredList
	// gets, when set, answers Get by "namespace/name" key; unset keys fall
	// through to a nil, nil no-op (existing List-only tests are unaffected).
	gets map[string]*unstructured.Unstructured

	lastNS       string
	lastSelector string
	lastResource string
}

func (l *listClient) Get(_ context.Context, _ schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if obj, ok := l.gets[ns+"/"+name]; ok {
		return obj, nil
	}
	return nil, nil
}

func (l *listClient) List(_ context.Context, gvr schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error) {
	l.lastNS, l.lastSelector, l.lastResource = ns, selector, gvr.Resource
	if list, ok := l.lists[gvr.Resource]; ok {
		return list, nil
	}
	return &unstructured.UnstructuredList{}, nil
}

func (l *listClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (l *listClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (l *listClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func (l *listClient) Logs(context.Context, string, string, string, int) (string, error) {
	return "", nil
}

func project(id, displayName string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": id},
		"spec":     map[string]any{"displayName": displayName},
	}}
}

func TestResolveProjectByExactID(t *testing.T) {
	t.Parallel()
	client := &listClient{lists: map[string]*unstructured.UnstructuredList{
		"projects": {Items: []unstructured.Unstructured{project("p-1", "web"), project("p-2", "db")}},
	}}
	factory := stubFactory{c: client}

	id, err := ResolveProject(context.Background(), factory, "c-x", "p-1")
	require.NoError(t, err)
	assert.Equal(t, "p-1", id)
}

func TestResolveProjectByUniqueDisplayName(t *testing.T) {
	t.Parallel()
	client := &listClient{lists: map[string]*unstructured.UnstructuredList{
		"projects": {Items: []unstructured.Unstructured{project("p-1", "web"), project("p-2", "db")}},
	}}
	factory := stubFactory{c: client}

	id, err := ResolveProject(context.Background(), factory, "c-x", "web")
	require.NoError(t, err)
	assert.Equal(t, "p-1", id)
	assert.Equal(t, "c-x", client.lastNS, "projects are listed in the cluster-id namespace")
}

func TestResolveProjectAmbiguousDisplayName(t *testing.T) {
	t.Parallel()
	client := &listClient{lists: map[string]*unstructured.UnstructuredList{
		"projects": {Items: []unstructured.Unstructured{project("p-1", "web"), project("p-2", "web")}},
	}}
	factory := stubFactory{c: client}

	_, err := ResolveProject(context.Background(), factory, "c-x", "web")
	require.Error(t, err)
	cerr, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeAmbiguousSelection, cerr.Code)
	require.Len(t, cerr.Candidates, 2)
}

func TestResolveProjectIDCollidesWithDifferentProjectsDisplayName(t *testing.T) {
	t.Parallel()
	// project A's id equals project B's display name — an id-wins
	// short-circuit would silently resolve to A; ResolveCluster's collision
	// semantics say this is ambiguous instead.
	client := &listClient{lists: map[string]*unstructured.UnstructuredList{
		"projects": {Items: []unstructured.Unstructured{project("web", "A"), project("p-2", "web")}},
	}}
	factory := stubFactory{c: client}

	_, err := ResolveProject(context.Background(), factory, "c-x", "web")
	require.Error(t, err)
	cerr, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeAmbiguousSelection, cerr.Code)
	require.Len(t, cerr.Candidates, 2)
	assert.Contains(t, cerr.Message, "web")
}

func TestResolveProjectNotFound(t *testing.T) {
	t.Parallel()
	client := &listClient{lists: map[string]*unstructured.UnstructuredList{
		"projects": {Items: []unstructured.Unstructured{project("p-1", "web")}},
	}}
	factory := stubFactory{c: client}

	_, err := ResolveProject(context.Background(), factory, "c-x", "unknown")
	require.Error(t, err)
	cerr, ok := err.(*clierr.Error)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeNotFound, cerr.Code)
	assert.Contains(t, cerr.Hint, "rancher get projects --cluster")
}

func TestProjectNamespacesListsWithLabelSelector(t *testing.T) {
	t.Parallel()
	client := &listClient{lists: map[string]*unstructured.UnstructuredList{
		"namespaces": {Items: []unstructured.Unstructured{
			{Object: map[string]any{"metadata": map[string]any{"name": "ns-a"}}},
			{Object: map[string]any{"metadata": map[string]any{"name": "ns-b"}}},
		}},
	}}

	members, err := ProjectNamespaces(context.Background(), client, "p-1")
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"ns-a": true, "ns-b": true}, members)
	assert.Equal(t, "field.cattle.io/projectId=p-1", client.lastSelector)
	assert.Equal(t, "", client.lastNS, "namespaces are cluster-scoped")
}

func TestProjectNamespacesEmptySetIsNotAnError(t *testing.T) {
	t.Parallel()
	client := &listClient{lists: map[string]*unstructured.UnstructuredList{}}

	members, err := ProjectNamespaces(context.Background(), client, "p-1")
	require.NoError(t, err)
	assert.Empty(t, members)
}

func projectWithBackingNS(id, backingNS string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": id}}}
	if backingNS != "" {
		_ = unstructured.SetNestedField(obj.Object, backingNS, "status", "backingNamespace")
	}
	return obj
}

func TestBackingNamespaceUsesStatusField(t *testing.T) {
	t.Parallel()
	client := &listClient{gets: map[string]*unstructured.Unstructured{
		"c-x/p-1": projectWithBackingNS("p-1", "p-abc123"),
	}}
	factory := stubFactory{c: client}

	ns, statusEmpty, err := BackingNamespace(context.Background(), factory, "c-x", "p-1")
	require.NoError(t, err)
	assert.Equal(t, "p-abc123", ns)
	assert.False(t, statusEmpty)
}

func TestBackingNamespaceFallsBackToProjectID(t *testing.T) {
	t.Parallel()
	client := &listClient{gets: map[string]*unstructured.Unstructured{
		"c-x/p-1": projectWithBackingNS("p-1", ""),
	}}
	factory := stubFactory{c: client}

	ns, statusEmpty, err := BackingNamespace(context.Background(), factory, "c-x", "p-1")
	require.NoError(t, err)
	assert.Equal(t, "p-1", ns)
	assert.True(t, statusEmpty)
}

var _ engine.ClusterClient = (*listClient)(nil)
