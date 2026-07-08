package projects

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type recClient struct {
	created   *unstructured.Unstructured
	createdNS string
	dryRuns   []bool
}

func (r *recClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (r *recClient) List(context.Context, schema.GroupVersionResource, string) (*unstructured.UnstructuredList, error) {
	return nil, nil
}
func (r *recClient) Create(_ context.Context, _ schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	r.created, r.createdNS = obj, ns
	r.dryRuns = append(r.dryRuns, dryRun)
	return obj, nil
}
func (r *recClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}
func (r *recClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	return nil
}

// stubFactory hands out a fixed ClusterClient for any cluster id.
type stubFactory struct{ c engine.ClusterClient }

func (f stubFactory) Cluster(context.Context, string) (engine.ClusterClient, error) { return f.c, nil }
func (f stubFactory) ContextCluster() (string, error)                               { return "", nil }
func (f stubFactory) Identity() (string, error)                                     { return "test-server", nil }

type in struct {
	s map[string]string
	a []string
}

func (i in) String(k string) string { return i.s[k] }
func (i in) Bool(string) bool       { return false }
func (i in) Args() []string         { return i.a }

func planProject(t *testing.T, cmd runtime.Command, input runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "create project must be a MutatingCommand")
	return mc.Plan(context.Background(), input)
}

func TestCreateProjectPlanBuildsObjectAndDryRuns(t *testing.T) {
	client := &recClient{}
	cmd := projectCmdFor(t, client)

	eff, exec, err := planProject(t, cmd, in{s: map[string]string{"cluster": "c-x"}, a: []string{"web"}})
	require.NoError(t, err)
	require.NotNil(t, exec)
	assert.Equal(t, []bool{true}, client.dryRuns, "Plan dry-runs")
	assert.Equal(t, safety.ActionCreate, eff.Action)
	assert.Equal(t, "projects/web", eff.Target)
	assert.Equal(t, "c-x", eff.Cluster) // Effect targets the downstream cluster, not the "local" proxy

	// object shape: management.cattle.io/v3 Project in the cluster-id namespace.
	assert.Equal(t, "c-x", client.createdNS)
	assert.Equal(t, "web", client.created.GetName())
	cn, _, _ := unstructured.NestedString(client.created.Object, "spec", "clusterName")
	assert.Equal(t, "c-x", cn)
	dn, _, _ := unstructured.NestedString(client.created.Object, "spec", "displayName")
	assert.Equal(t, "web", dn)
	assert.Equal(t, "management.cattle.io/v3", client.created.GetAPIVersion())
	assert.Equal(t, "Project", client.created.GetKind())
}

func TestCreateProjectExecutorWrites(t *testing.T) {
	client := &recClient{}
	cmd := projectCmdFor(t, client)
	_, exec, err := planProject(t, cmd, in{s: map[string]string{"cluster": "c-x"}, a: []string{"web"}})
	require.NoError(t, err)

	res, err := exec(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "Project", res.Kind)
	assert.Equal(t, []bool{true, false}, client.dryRuns, "Plan dry-runs, then the executor writes for real")
}

func TestCreateProjectRequiresClusterAndName(t *testing.T) {
	client := &recClient{}
	cmd := projectCmdFor(t, client)
	// missing cluster
	_, _, err := planProject(t, cmd, in{a: []string{"web"}})
	require.Error(t, err)
	// missing name
	_, _, err = planProject(t, cmd, in{s: map[string]string{"cluster": "c-x"}})
	require.Error(t, err)
}

func projectCmdFor(t *testing.T, c engine.ClusterClient) runtime.Command {
	t.Helper()
	return NewCreate(stubFactory{c})
}
