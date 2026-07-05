package clusters

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// memRepo serves canned cluster objects for List.
type memRepo struct{ items []unstructured.Unstructured }

func (m *memRepo) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	return &m.items[0], nil
}
func (m *memRepo) List(context.Context, schema.GroupVersionResource, string) (*unstructured.UnstructuredList, error) {
	return &unstructured.UnstructuredList{Items: m.items}, nil
}
func (m *memRepo) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (m *memRepo) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (m *memRepo) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

func clusterObj(name, display, state string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"displayName": display},
		"status":   map[string]any{"conditions": []any{}, "state": state},
	}}
}

func factory(r engine.Repo) engine.RepoFactory {
	return func(string) (engine.Repo, error) { return r, nil }
}

func TestGetClusterList(t *testing.T) {
	repo := &memRepo{items: []unstructured.Unstructured{
		clusterObj("local", "local", "active"),
		clusterObj("c-m-abc", "prod", "active"),
	}}
	cmd := NewGet(factory(repo), "crd", "beta")

	d := cmd.Describe()
	assert.Equal(t, "get cluster", d.Name)
	assert.Equal(t, runtime.ReadOnly, d.CommandType)

	res, err := cmd.Execute(context.Background(), input{}, safety.Effect{})
	require.NoError(t, err)
	assert.Equal(t, "crd", res.Tier)
	assert.Equal(t, "beta", res.Stability)
	tab, ok := res.Data.(output.Tabular)
	require.True(t, ok)
	assert.Equal(t, []string{"NAME", "DISPLAY", "STATE"}, tab.Columns())
	assert.Len(t, tab.Rows(), 2)
	assert.Equal(t, []string{"c-m-abc", "prod", "active"}, tab.Rows()[1])
}

// input is a minimal runtime.Input for tests.
type input struct{ args []string }

func (i input) String(string) string { return "" }
func (i input) Bool(string) bool     { return false }
func (i input) Args() []string       { return i.args }

// NewGet must accept a nil factory for Describe().
func TestGetClusterDescribeNilFactory(t *testing.T) {
	assert.Equal(t, "get cluster", NewGet(nil, "crd", "beta").Describe().Name)
}
