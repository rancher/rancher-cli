package projects

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
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
func (r *recClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	return nil, nil
}
func (r *recClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
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

// stubFactory hands out a fixed ClusterClient for any cluster id and resolves
// display names via an optional map (unmapped names pass through unchanged).
type stubFactory struct {
	c       engine.ClusterClient
	resolve map[string]string
}

func (f stubFactory) Cluster(context.Context, string) (engine.ClusterClient, error) { return f.c, nil }
func (f stubFactory) ContextCluster() (string, error)                               { return "", nil }
func (f stubFactory) ContextProject() (string, error)                               { return "", nil }
func (f stubFactory) ResolveCluster(_ context.Context, s string) (string, error) {
	if id, ok := f.resolve[s]; ok {
		return id, nil
	}
	return s, nil
}

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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	client := &recClient{}
	cmd := projectCmdFor(t, client)
	// missing cluster
	_, _, err := planProject(t, cmd, in{a: []string{"web"}})
	require.Error(t, err)
	// missing name
	_, _, err = planProject(t, cmd, in{s: map[string]string{"cluster": "c-x"}})
	require.Error(t, err)
}

func TestCreateProjectResolvesClusterDisplayName(t *testing.T) {
	t.Parallel()
	client := &recClient{}
	cmd := NewCreate(stubFactory{c: client, resolve: map[string]string{"prod": "c-abc"}})

	eff, _, err := planProject(t, cmd, in{s: map[string]string{"cluster": "prod"}, a: []string{"web"}})
	require.NoError(t, err)
	assert.Equal(t, "c-abc", eff.Cluster, "Effect targets the resolved cluster id")
	assert.Equal(t, "c-abc", client.createdNS, "namespace is the resolved cluster id")
	cn, _, _ := unstructured.NestedString(client.created.Object, "spec", "clusterName")
	assert.Equal(t, "c-abc", cn)
}

func projectCmdFor(t *testing.T, c engine.ClusterClient) runtime.Command {
	t.Helper()
	return NewCreate(stubFactory{c: c})
}

func TestCreateProjectQuotaFlagsPopulateBothSpecs(t *testing.T) {
	t.Parallel()
	client := &recClient{}
	cmd := projectCmdFor(t, client)

	_, _, err := planProject(t, cmd, in{s: map[string]string{
		"cluster":                 "c-x",
		"quota-limit":             "limitsCpu=2,limitsMemory=4Gi",
		"namespace-default-quota": "limitsCpu=1,limitsMemory=2Gi",
	}, a: []string{"web"}})
	require.NoError(t, err)

	limitCPU, _, _ := unstructured.NestedString(client.created.Object, "spec", "resourceQuota", "limit", "limitsCpu")
	assert.Equal(t, "2", limitCPU)
	limitMem, _, _ := unstructured.NestedString(client.created.Object, "spec", "resourceQuota", "limit", "limitsMemory")
	assert.Equal(t, "4Gi", limitMem)
	nsCPU, _, _ := unstructured.NestedString(client.created.Object, "spec", "namespaceDefaultResourceQuota", "limit", "limitsCpu")
	assert.Equal(t, "1", nsCPU)
	nsMem, _, _ := unstructured.NestedString(client.created.Object, "spec", "namespaceDefaultResourceQuota", "limit", "limitsMemory")
	assert.Equal(t, "2Gi", nsMem)
}

func TestCreateProjectWithoutQuotaFlagsHasNoQuotaSpecs(t *testing.T) {
	t.Parallel()
	client := &recClient{}
	cmd := projectCmdFor(t, client)

	_, _, err := planProject(t, cmd, in{s: map[string]string{"cluster": "c-x"}, a: []string{"web"}})
	require.NoError(t, err)

	_, found, _ := unstructured.NestedMap(client.created.Object, "spec", "resourceQuota")
	assert.False(t, found, "no quota flags means no empty quota spec")
	_, found, _ = unstructured.NestedMap(client.created.Object, "spec", "namespaceDefaultResourceQuota")
	assert.False(t, found)
}

func TestCreateProjectOneQuotaFlagWithoutTheOtherIsValidation(t *testing.T) {
	t.Parallel()
	client := &recClient{}
	cmd := projectCmdFor(t, client)

	_, _, err := planProject(t, cmd, in{s: map[string]string{
		"cluster":     "c-x",
		"quota-limit": "limitsCpu=2",
	}, a: []string{"web"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "together")
}

func TestCreateProjectQuotaKeySetMismatchIsValidation(t *testing.T) {
	t.Parallel()
	client := &recClient{}
	cmd := projectCmdFor(t, client)

	_, _, err := planProject(t, cmd, in{s: map[string]string{
		"cluster":                 "c-x",
		"quota-limit":             "limitsCpu=2,limitsMemory=4Gi",
		"namespace-default-quota": "limitsCpu=1",
	}, a: []string{"web"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "limitsMemory")
}

func TestCreateProjectQuotaUnknownKeyIsValidation(t *testing.T) {
	t.Parallel()
	client := &recClient{}
	cmd := projectCmdFor(t, client)

	_, _, err := planProject(t, cmd, in{s: map[string]string{
		"cluster":                 "c-x",
		"quota-limit":             "limitsCPU=2",
		"namespace-default-quota": "limitsCPU=1",
	}, a: []string{"web"}})
	require.Error(t, err)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "limitsCPU")
	assert.Contains(t, ce.Hint, "limitsCpu")
}

func TestCreateProjectQuotaMalformedPairIsValidation(t *testing.T) {
	t.Parallel()
	cases := []string{"foo", "=x", "limitsCpu="}
	for _, pair := range cases {
		client := &recClient{}
		cmd := projectCmdFor(t, client)
		_, _, err := planProject(t, cmd, in{s: map[string]string{
			"cluster":                 "c-x",
			"quota-limit":             pair,
			"namespace-default-quota": pair,
		}, a: []string{"web"}})
		require.Error(t, err, "pair %q", pair)
		var ce *clierr.Error
		require.True(t, errors.As(err, &ce))
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	}
}

// memberClient fakes the pieces create-secret exercises: List (projects,
// keyed by GVR resource), Get (the project object, for BackingNamespace),
// Create (recorded), and Delete (recorded per call).
type memberClient struct {
	projects *unstructured.UnstructuredList
	users    *unstructured.UnstructuredList
	prtbs    *unstructured.UnstructuredList
	// gets answers Get by "namespace/name" key.
	gets map[string]*unstructured.Unstructured

	created       *unstructured.Unstructured
	createdNS     string
	createDryRuns []bool

	deletedNames  []string
	deletedNS     []string
	deletedRVs    []string
	deleteDryRuns []bool
}

func (m *memberClient) Get(_ context.Context, _ schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	if obj, ok := m.gets[ns+"/"+name]; ok {
		return obj, nil
	}
	return nil, clierr.New(clierr.CodeNotFound, "not found: "+name)
}

func (m *memberClient) List(_ context.Context, gvr schema.GroupVersionResource, _, _ string) (*unstructured.UnstructuredList, error) {
	switch gvr.Resource {
	case "projects":
		if m.projects != nil {
			return m.projects, nil
		}
	case "users":
		if m.users != nil {
			return m.users, nil
		}
	case "projectroletemplatebindings":
		if m.prtbs != nil {
			return m.prtbs, nil
		}
	}
	return &unstructured.UnstructuredList{}, nil
}

func (m *memberClient) Create(_ context.Context, _ schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	m.created, m.createdNS = obj, ns
	m.createDryRuns = append(m.createDryRuns, dryRun)
	return obj, nil
}

func (m *memberClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	return nil, nil
}

func (m *memberClient) Delete(_ context.Context, _ schema.GroupVersionResource, ns, name, rv string, dryRun bool) error {
	m.deletedNames = append(m.deletedNames, name)
	m.deletedNS = append(m.deletedNS, ns)
	m.deletedRVs = append(m.deletedRVs, rv)
	m.deleteDryRuns = append(m.deleteDryRuns, dryRun)
	return nil
}

func (m *memberClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
}

// memberIn is a runtime.Input fake supporting bool flags. The shared in type
// hardcodes Bool to false; secret_test.go needs a working --project-scoped flag.
type memberIn struct {
	s map[string]string
	b map[string]bool
	a []string
}

func (i memberIn) String(k string) string { return i.s[k] }
func (i memberIn) Bool(k string) bool     { return i.b[k] }
func (i memberIn) Args() []string         { return i.a }

func planMember(t *testing.T, cmd runtime.Command, input runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "member commands must be MutatingCommand")
	return mc.Plan(context.Background(), input)
}
