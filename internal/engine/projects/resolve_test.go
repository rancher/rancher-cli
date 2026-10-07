package projects

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

// listClient is a minimal engine.ClusterClient fake that answers Get by
// "namespace/name" key; List always returns an empty list.
type listClient struct {
	// gets answers Get by "namespace/name" key; unset keys fall through to
	// a nil, nil no-op.
	gets map[string]*unstructured.Unstructured
	// getErrs, when set, returns the error for "namespace/name" key.
	getErrs map[string]error
}

func (l *listClient) Get(_ context.Context, _ schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	k := ns + "/" + name
	if err, ok := l.getErrs[k]; ok {
		return nil, err
	}
	if obj, ok := l.gets[k]; ok {
		return obj, nil
	}
	return nil, nil
}

func (l *listClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
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

func (l *listClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
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

	ns, statusEmpty, err := BackingNamespace(t.Context(), factory, "c-x", "p-1")
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

	ns, statusEmpty, err := BackingNamespace(t.Context(), factory, "c-x", "p-1")
	require.NoError(t, err)
	assert.Equal(t, "p-1", ns)
	assert.True(t, statusEmpty)
}

func TestBackingNamespaceNotFoundCurated(t *testing.T) {
	t.Parallel()
	projectKind, _ := kinds.Lookup("project")
	client := &listClient{getErrs: map[string]error{"c-x/p-gone": kinds.NotFoundCurated(projectKind, "p-gone", "")}}
	factory := stubFactory{c: client}

	_, _, err := BackingNamespace(t.Context(), factory, "c-x", "p-gone")
	require.Error(t, err)

	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "project not found: p-gone", ce.Message)
	assert.NotEmpty(t, ce.Hint, "curated error must carry a hint")
}

func TestBackingNamespaceNotFoundWithHintPassesThrough(t *testing.T) {
	t.Parallel()
	alreadyCurated := clierr.New(clierr.CodeNotFound, "project not found: p-gone").
		WithHint("list with: rancher get projects")
	client := &listClient{getErrs: map[string]error{"c-x/p-gone": alreadyCurated}}
	factory := stubFactory{c: client}

	_, _, err := BackingNamespace(t.Context(), factory, "c-x", "p-gone")
	require.Error(t, err)
	assert.Equal(t, alreadyCurated, err, "already-curated error must pass through unchanged")
}

var _ engine.ClusterClient = (*listClient)(nil)

func (*listClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*listClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}
