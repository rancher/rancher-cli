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

	lastNS       string
	lastSelector string
	lastResource string
}

func (l *listClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
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

var _ engine.ClusterClient = (*listClient)(nil)
