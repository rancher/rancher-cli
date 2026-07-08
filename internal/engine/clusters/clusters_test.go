package clusters

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// memClient serves canned cluster objects for List.
type memClient struct{ items []unstructured.Unstructured }

func (m *memClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	return &m.items[0], nil
}
func (m *memClient) List(context.Context, schema.GroupVersionResource, string) (*unstructured.UnstructuredList, error) {
	return &unstructured.UnstructuredList{Items: m.items}, nil
}
func (m *memClient) Create(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (m *memClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (m *memClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

// stubFactory hands out a fixed ClusterClient for any cluster id.
type stubFactory struct{ c engine.ClusterClient }

func (f stubFactory) Cluster(context.Context, string) (engine.ClusterClient, error) { return f.c, nil }

func clusterObj(name, display, state string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"displayName": display},
		"status":   map[string]any{"conditions": []any{}, "state": state},
	}}
}

func execGet(t *testing.T, cmd runtime.Command, in runtime.Input) runtime.Result {
	t.Helper()
	rc, ok := cmd.(runtime.ReadCommand)
	require.True(t, ok, "get cluster must be a ReadCommand")
	res, err := rc.Execute(context.Background(), in, nil)
	require.NoError(t, err)
	return res
}

func TestGetClusterList(t *testing.T) {
	client := &memClient{items: []unstructured.Unstructured{
		clusterObj("local", "local", "active"),
		clusterObj("c-m-abc", "prod", "active"),
	}}
	cmd := NewGet(stubFactory{client})

	d := cmd.Describe()
	assert.Equal(t, "get cluster", d.Name)
	assert.Equal(t, runtime.ReadOnly, d.CommandType)

	res := execGet(t, cmd, input{})
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
	assert.Equal(t, "get cluster", NewGet(nil).Describe().Name)
}

// An empty positional (`rancher get cluster ""`) must list, not Get("") — a real
// API server rejects an empty name with an opaque error.
func TestGetClusterEmptyArgLists(t *testing.T) {
	client := &memClient{items: []unstructured.Unstructured{clusterObj("local", "local", "active")}}
	res := execGet(t, NewGet(stubFactory{client}), input{args: []string{""}})
	assert.Equal(t, "ClusterList", res.Kind)
}
