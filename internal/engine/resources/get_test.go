package resources

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// memClient serves canned objects and records the namespaces/names it is asked
// for, so routing tests can assert what reached the client.
type memClient struct {
	items   []unstructured.Unstructured
	listNS  []string
	getNS   []string
	getName []string
}

func (m *memClient) Get(_ context.Context, _ schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	m.getNS = append(m.getNS, ns)
	m.getName = append(m.getName, name)
	return &m.items[0], nil
}
func (m *memClient) List(_ context.Context, _ schema.GroupVersionResource, ns string) (*unstructured.UnstructuredList, error) {
	m.listNS = append(m.listNS, ns)
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

// recFactory records the cluster ids clients are requested for and resolves
// display names via an optional map.
type recFactory struct {
	client       engine.ClusterClient
	resolve      map[string]string
	ctxCluster   string
	clusterCalls []string
}

func (f *recFactory) Cluster(_ context.Context, id string) (engine.ClusterClient, error) {
	f.clusterCalls = append(f.clusterCalls, id)
	return f.client, nil
}
func (f *recFactory) ContextCluster() (string, error) { return f.ctxCluster, nil }
func (f *recFactory) ResolveCluster(_ context.Context, s string) (string, error) {
	if s == "" || s == engine.LocalCluster {
		return engine.LocalCluster, nil
	}
	if id, ok := f.resolve[s]; ok {
		return id, nil
	}
	return s, nil
}

type input struct {
	args []string
	s    map[string]string
}

func (i input) String(k string) string { return i.s[k] }
func (i input) Bool(string) bool       { return false }
func (i input) Args() []string         { return i.args }

func clusterObj(name, display, state string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"displayName": display},
		"status":   map[string]any{"state": state},
	}}
}

func podObj(name string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"status":   map[string]any{"phase": "Running"},
	}}
}

func projectObj(name, display, cluster string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"displayName": display, "clusterName": cluster},
	}}
}

func mustKind(t *testing.T, name string) kinds.Kind {
	t.Helper()
	k, ok := kinds.Lookup(name)
	require.True(t, ok, "kind %q must exist", name)
	return k
}

func execGet(t *testing.T, cmd runtime.Command, in runtime.Input) runtime.Result {
	t.Helper()
	rc, ok := cmd.(runtime.ReadCommand)
	require.True(t, ok, "get must be a ReadCommand")
	res, err := rc.Execute(context.Background(), in, nil)
	require.NoError(t, err)
	return res
}

func TestGetClusterListsWithColumns(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{
		clusterObj("local", "local", "active"),
		clusterObj("c-m-abc", "prod", "active"),
	}}
	cmd := NewGet(mustKind(t, "cluster"), &recFactory{client: client})

	d := cmd.Describe()
	assert.Equal(t, "get cluster", d.Name)
	assert.Equal(t, runtime.ReadOnly, d.CommandType)

	res := execGet(t, cmd, input{})
	assert.Equal(t, "ClusterList", res.Kind)
	tab, ok := res.Data.(output.Tabular)
	require.True(t, ok)
	assert.Equal(t, []string{"NAME", "DISPLAY", "STATE"}, tab.Columns())
	require.Len(t, tab.Rows(), 2)
	assert.Equal(t, []string{"c-m-abc", "prod", "active"}, tab.Rows()[1])
}

func TestGetClusterSingle(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{clusterObj("c-m-abc", "prod", "active")}}
	res := execGet(t, NewGet(mustKind(t, "cluster"), &recFactory{client: client}), input{args: []string{"prod"}})
	assert.Equal(t, "Cluster", res.Kind)
	assert.Equal(t, []string{"prod"}, client.getName)
	assert.Equal(t, []string{""}, client.getNS, "clusters are cluster-scoped")
}

func TestGetClusterEmptyArgLists(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{clusterObj("local", "local", "active")}}
	res := execGet(t, NewGet(mustKind(t, "cluster"), &recFactory{client: client}), input{args: []string{""}})
	assert.Equal(t, "ClusterList", res.Kind, "empty positional lists, never Get(\"\")")
}

func TestGetDescribeNilFactory(t *testing.T) {
	t.Parallel()
	d := NewGet(mustKind(t, "pod"), nil).Describe()
	assert.Equal(t, "get pod", d.Name)
	assert.Equal(t, []string{"pods", "po"}, d.Aliases)
}

func TestGetPodTargetsResolvedCluster(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{podObj("web")}}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	res := execGet(t, NewGet(mustKind(t, "pod"), fac), input{s: map[string]string{"cluster": "prod", "namespace": "team-a"}})
	assert.Equal(t, "PodList", res.Kind)
	assert.Equal(t, []string{"c-abc"}, fac.clusterCalls, "client requested for the resolved cluster id")
	assert.Equal(t, []string{"team-a"}, client.listNS, "--namespace honored")
}

func TestGetManagementKindIgnoresClusterFlagRouting(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "project")

	// --cluster resolves to the namespace; the client is always the local proxy.
	client := &memClient{items: []unstructured.Unstructured{projectObj("p-1", "web", "c-abc")}}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-abc"}}
	res := execGet(t, NewGet(k, fac), input{s: map[string]string{"cluster": "prod"}})
	assert.Equal(t, "ProjectList", res.Kind)
	assert.Equal(t, []string{engine.LocalCluster}, fac.clusterCalls, "management kind served by the local proxy")
	assert.Equal(t, []string{"c-abc"}, client.listNS, "--cluster becomes the resolved namespace")

	// F3: get by name with an empty --cluster must NOT silently target "local".
	byName := NewGet(k, &recFactory{client: &memClient{items: []unstructured.Unstructured{projectObj("p-1", "web", "c-abc")}}})
	rc := byName.(runtime.ReadCommand)
	_, err := rc.Execute(context.Background(), input{args: []string{"web"}}, nil)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce), "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	// No name + empty --cluster lists across all clusters (namespace "").
	listAll := &memClient{items: []unstructured.Unstructured{projectObj("p-1", "web", "c-abc")}}
	fac2 := &recFactory{client: listAll}
	execGet(t, NewGet(k, fac2), input{})
	assert.Equal(t, []string{engine.LocalCluster}, fac2.clusterCalls)
	assert.Equal(t, []string{""}, listAll.listNS, "empty --cluster lists across all clusters")
}
