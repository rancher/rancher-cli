package projects

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/selfuser"
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
func (r *recClient) Create(_ context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	if gvr == selfuser.GVR {
		answer := obj.DeepCopy()
		_ = unstructured.SetNestedField(answer.Object, "u-self", "status", "userID")
		return answer, nil
	}
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

func (i in) String(k string) string  { return i.s[k] }
func (i in) Bool(string) bool        { return false }
func (i in) Strings(string) []string { return nil }
func (i in) IsSet(string) bool       { return false }
func (i in) Args() []string          { return i.a }

func planProject(t *testing.T, cmd runtime.Command, input runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "create project must be a MutatingCommand")
	return mc.Plan(t.Context(), input)
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

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Project", res.Kind)
	assert.Equal(t, []bool{true, false}, client.dryRuns, "Plan dry-runs, then the executor writes for real")
}

// TestCreateProjectRequiresClusterAndDisplayNameWithoutID asserts a missing
// cluster is refused, and so is a missing display name when no id is given.
func TestCreateProjectRequiresClusterAndDisplayNameWithoutID(t *testing.T) {
	t.Parallel()
	client := &recClient{}
	cmd := projectCmdFor(t, client)
	// missing cluster
	_, _, err := planProject(t, cmd, in{a: []string{"web"}})
	require.Error(t, err)
	// missing display name without an id
	_, _, err = planProject(t, cmd, in{s: map[string]string{"cluster": "c-x"}})
	require.ErrorContains(t, err, "--display-name is required")
	assert.Empty(t, client.dryRuns)
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
	require.ErrorAs(t, err, &ce)
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
	require.ErrorAs(t, err, &ce)
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
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "limitsCPU")
	assert.Contains(t, ce.Message, "limitsCpu")
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
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	}
}

// TestCreateProjectContainerDefaultResourceLimitWritesSpec asserts the flag
// writes all four keys flat under spec.containerDefaultResourceLimit (no
// "limit" wrapper, unlike the quota specs) and needs no quota flag.
func TestCreateProjectContainerDefaultResourceLimitWritesSpec(t *testing.T) {
	t.Parallel()
	client := &recClient{}
	cmd := projectCmdFor(t, client)

	_, _, err := planProject(t, cmd, in{s: map[string]string{
		"cluster":                          "c-x",
		"container-default-resource-limit": "limitsCpu=100m,limitsMemory=64Mi,requestsCpu=50m,requestsMemory=32Mi",
	}, a: []string{"web"}})
	require.NoError(t, err)
	assert.Equal(t, []bool{true}, client.dryRuns, "the field reaches the server-side dry-run")

	got, found, err := unstructured.NestedStringMap(client.created.Object, "spec", "containerDefaultResourceLimit")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, map[string]string{
		"limitsCpu": "100m", "limitsMemory": "64Mi", "requestsCpu": "50m", "requestsMemory": "32Mi",
	}, got)
	_, found, _ = unstructured.NestedMap(client.created.Object, "spec", "resourceQuota")
	assert.False(t, found, "the container limit does not imply a quota")
}

func TestCreateProjectWithoutContainerLimitFlagHasNoContainerLimit(t *testing.T) {
	t.Parallel()
	client := &recClient{}
	cmd := projectCmdFor(t, client)

	_, _, err := planProject(t, cmd, in{s: map[string]string{"cluster": "c-x"}, a: []string{"web"}})
	require.NoError(t, err)
	_, found, _ := unstructured.NestedFieldNoCopy(client.created.Object, "spec", "containerDefaultResourceLimit")
	assert.False(t, found, "no flag means no empty container limit")
}

// TestCreateProjectContainerLimitBadInputIsValidation asserts an unknown or
// malformed pair fails before any request, and an unknown key's message
// names the valid keys.
func TestCreateProjectContainerLimitBadInputIsValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, value string
		wantInMsg   []string
	}{
		{"unknown key, wrong case", "limitsCPU=1", []string{`"limitsCPU"`, "limitsCpu, limitsMemory, requestsCpu, requestsMemory"}},
		{"quota-only key", "pods=10", []string{`"pods"`, "allowed keys"}},
		{"unknown after a valid key", "limitsCpu=1,storage=1Gi", []string{`"storage"`}},
		{"missing equals", "limitsCpu", []string{"malformed", `"limitsCpu"`}},
		{"empty key", "=1", []string{"malformed"}},
		{"empty value", "limitsCpu=", []string{"malformed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &recClient{}
			cmd := projectCmdFor(t, client)
			_, _, err := planProject(t, cmd, in{s: map[string]string{
				"cluster":                          "c-x",
				"container-default-resource-limit": tc.value,
			}, a: []string{"web"}})
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			for _, want := range tc.wantInMsg {
				require.ErrorContains(t, err, want)
			}
			assert.Empty(t, client.dryRuns, "validation fails before any request")
		})
	}
}

// TestCreateProjectContainerLimitDocs asserts the flag's help line stays
// short while the schema-only command desc names every accepted key.
func TestCreateProjectContainerLimitDocs(t *testing.T) {
	t.Parallel()
	d := NewCreate(stubFactory{}).Describe()
	i := slices.IndexFunc(d.Flags, func(a runtime.ArgSpec) bool { return a.Name == "container-default-resource-limit" })
	require.GreaterOrEqual(t, i, 0)
	assert.Equal(t, "default container resource requests and limits as `key=value` pairs", d.Flags[i].Desc)
	for _, k := range ContainerDefaultResourceLimitKeys() {
		assert.Contains(t, d.Desc, k, "the schema desc names every key")
	}
	assert.Contains(t, d.Desc, "--container-default-resource-limit")
}

// TestKeyValueFlagsRefuseARepeatedKey asserts every flag on the shared
// key=value parser refuses a key given twice, naming the flag and the key,
// before any request.
func TestKeyValueFlagsRefuseARepeatedKey(t *testing.T) {
	t.Parallel()
	projectCases := []struct {
		flag  string
		flags map[string]string
	}{
		{"quota-limit", map[string]string{
			"quota-limit":             "limitsCpu=1,limitsCpu=2",
			"namespace-default-quota": "limitsCpu=1",
		}},
		{"namespace-default-quota", map[string]string{
			"quota-limit":             "limitsCpu=2",
			"namespace-default-quota": "limitsCpu=1,limitsMemory=1Gi,limitsCpu=2",
		}},
		{"container-default-resource-limit", map[string]string{
			"container-default-resource-limit": "limitsCpu=1,limitsCpu=2",
		}},
	}
	for _, tc := range projectCases {
		t.Run("create project --"+tc.flag, func(t *testing.T) {
			t.Parallel()
			client := &recClient{}
			flags := map[string]string{"cluster": "c-x"}
			maps.Copy(flags, tc.flags)
			_, _, err := planProject(t, projectCmdFor(t, client), in{s: flags, a: []string{"web"}})
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			require.ErrorContains(t, err, `the key "limitsCpu" appears more than once in --`+tc.flag)
			assert.Empty(t, client.dryRuns, "refused before any request")
		})
	}

	t.Run("create secret --from-literal", func(t *testing.T) {
		t.Parallel()
		client := &memberClient{}
		_, _, err := planMember(t, t.Context(), NewCreateSecret(stubFactory{c: client}), memberIn{
			s: map[string]string{"namespace": "team-a", "cluster": "c-x", "from-literal": "token=a,other=,token=b"},
			a: []string{"app-secret"},
		})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		require.ErrorContains(t, err, `the key "token" appears more than once in --from-literal`)
		assert.Nil(t, client.created, "refused before any request")
	})
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

// memberIn is the runtime.Input fake of the create secret tests.
type memberIn struct {
	s map[string]string
	a []string
}

func (i memberIn) String(k string) string  { return i.s[k] }
func (i memberIn) Bool(string) bool        { return false }
func (i memberIn) Strings(string) []string { return nil }
func (i memberIn) IsSet(string) bool       { return false }
func (i memberIn) Args() []string          { return i.a }

func planMember(t *testing.T, ctx context.Context, cmd runtime.Command, input runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	t.Helper()
	mc, ok := cmd.(runtime.MutatingCommand)
	require.True(t, ok, "member commands must be MutatingCommand")
	return mc.Plan(ctx, input)
}

func (*recClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*recClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}

func (*memberClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*memberClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}

func (f stubFactory) ImpersonatedCluster(ctx context.Context, id string, _ engine.Impersonation) (engine.ClusterClient, error) {
	return f.Cluster(ctx, id)
}

// callLog records, in order, every lookup and request a fake receives.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *callLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// countingFactory is a ClusterClientFactory whose every call is logged.
type countingFactory struct {
	stubFactory
	log *callLog
}

func (f countingFactory) Cluster(ctx context.Context, id string) (engine.ClusterClient, error) {
	f.log.add("Cluster " + id)
	return f.stubFactory.Cluster(ctx, id)
}

func (f countingFactory) ResolveCluster(ctx context.Context, s string) (string, error) {
	f.log.add("ResolveCluster " + s)
	return f.stubFactory.ResolveCluster(ctx, s)
}

// generatingClient responds to a create the way the server responds to a
// Project that carries metadata.generateName: the dry-run with the name
// p-dry01 (the Kubernetes API server generates it, because the Rancher
// webhook leaves dry-run requests alone) and the real create with the name
// p-real1 (the Rancher webhook generates it). An object that carries
// metadata.name is returned as sent. Every call is logged and the object of
// each create is kept.
type generatingClient struct {
	log     *callLog
	dryObj  *unstructured.Unstructured
	realObj *unstructured.Unstructured
}

func (g *generatingClient) Get(context.Context, schema.GroupVersionResource, string, string) (*unstructured.Unstructured, error) {
	g.log.add("Get")
	return nil, nil
}

func (g *generatingClient) List(context.Context, schema.GroupVersionResource, string, string) (*unstructured.UnstructuredList, error) {
	g.log.add("List")
	return nil, nil
}

func (g *generatingClient) Create(_ context.Context, gvr schema.GroupVersionResource, _ string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	g.log.add("Create " + gvr.Resource)
	if gvr == selfuser.GVR {
		response := obj.DeepCopy()
		_ = unstructured.SetNestedField(response.Object, "u-self", "status", "userID")
		return response, nil
	}
	if dryRun {
		g.dryObj = obj
	} else {
		g.realObj = obj
	}
	response := obj.DeepCopy()
	if obj.GetName() == "" && obj.GetGenerateName() != "" {
		if dryRun {
			response.SetName("p-dry01")
		} else {
			response.SetName("p-real1")
		}
	}
	return response, nil
}

func (g *generatingClient) Update(context.Context, schema.GroupVersionResource, string, *unstructured.Unstructured, bool) (*unstructured.Unstructured, error) {
	g.log.add("Update")
	return nil, nil
}

func (g *generatingClient) Delete(context.Context, schema.GroupVersionResource, string, string, string, bool) error {
	g.log.add("Delete")
	return nil
}

func (g *generatingClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	g.log.add("Logs")
	return "", nil
}

func (g *generatingClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	g.log.add("PostReview")
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (g *generatingClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	g.log.add("ServerResources")
	return nil, errors.New("ServerResources not scripted in this fake")
}

// generatingFixture is a command wired to a generatingClient behind a
// counting factory.
func generatingFixture(t *testing.T) (runtime.Command, *generatingClient, *callLog) {
	t.Helper()
	log := &callLog{}
	client := &generatingClient{log: log}
	return NewCreate(countingFactory{c: client, log: log}), client, log
}

// TestCreateProjectIDWithDisplayName asserts an id and --display-name give
// metadata.name the id and spec.displayName the display name.
func TestCreateProjectIDWithDisplayName(t *testing.T) {
	t.Parallel()
	client := &recClient{}
	cmd := projectCmdFor(t, client)

	eff, _, err := planProject(t, cmd, in{s: map[string]string{"cluster": "c-x", "display-name": "Web tier"}, a: []string{"web"}})
	require.NoError(t, err)
	assert.Equal(t, "web", client.created.GetName())
	dn, _, _ := unstructured.NestedString(client.created.Object, "spec", "displayName")
	assert.Equal(t, "Web tier", dn)
	assert.Equal(t, "projects/web", eff.Target, "with an id the preview shows the id")
	assert.Equal(t, "web", eff.DisplayName)
	assert.Equal(t, "project", eff.DisplayKind)
}

// TestCreateProjectWithoutIDSendsGenerateName asserts a create with no id:
// the object carries metadata.generateName p- and no metadata.name, the
// preview shows p-* in place of the id, and the --apply result shows the id
// the server generated for the real create.
func TestCreateProjectWithoutIDSendsGenerateName(t *testing.T) {
	t.Parallel()
	cmd, client, _ := generatingFixture(t)

	eff, exec, err := planProject(t, cmd, in{s: map[string]string{"cluster": "c-x", "display-name": "Web tier"}})
	require.NoError(t, err)
	require.NotNil(t, client.dryObj)

	_, hasName, err := unstructured.NestedFieldNoCopy(client.dryObj.Object, "metadata", "name")
	require.NoError(t, err)
	assert.False(t, hasName, "the object carries no metadata.name key")
	gn, _, _ := unstructured.NestedString(client.dryObj.Object, "metadata", "generateName")
	assert.Equal(t, "p-", gn)
	dn, _, _ := unstructured.NestedString(client.dryObj.Object, "spec", "displayName")
	assert.Equal(t, "Web tier", dn)
	annotation, _, _ := unstructured.NestedString(client.dryObj.Object, "metadata", "annotations", "field.cattle.io/creatorId")
	assert.Equal(t, "u-self", annotation, "a create with no id sets the creator annotation")

	assert.Equal(t, safety.ActionCreate, eff.Action)
	assert.Equal(t, "projects/p-*", eff.Target)
	assert.Equal(t, "p-*", eff.DisplayName)
	assert.Equal(t, "project", eff.DisplayKind)
	assert.Equal(t, "c-x", eff.Cluster)
	assert.True(t, slices.ContainsFunc(eff.Diff, func(e safety.DiffEntry) bool { return e.Path == "/spec/displayName" }), "the diff holds /spec/displayName: %v", eff.Diff)

	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "project/p-real1 created\n", res.Text)
	assert.NotContains(t, res.Text, "*")
	assert.NotContains(t, res.Text, "p-dry01")
	name, _, _ := unstructured.NestedString(res.Data.(map[string]any), "metadata", "name")
	assert.Equal(t, "p-real1", name, "the result shows the generated id")
	assert.Equal(t, "p-", client.realObj.GetGenerateName())
}

// TestCreateProjectWithoutIDRequiresDisplayName asserts no id and no
// --display-name is refused before any lookup or request.
func TestCreateProjectWithoutIDRequiresDisplayName(t *testing.T) {
	t.Parallel()
	cmd, _, log := generatingFixture(t)

	_, _, err := planProject(t, cmd, in{s: map[string]string{"cluster": "c-x"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, 2, ce.ExitCode())
	require.ErrorContains(t, err, "--display-name is required when no project id is given")
	assert.Equal(t, "--display-name is required when no project id is given; pass --display-name <name>, or pass the project id as the argument", ce.Message)
	assert.Empty(t, log.all(), "refused before any lookup or request")

	// With no cluster either, the display name is the first refusal.
	_, _, err = planProject(t, cmd, in{})
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "--display-name is required when no project id is given; pass --display-name <name>, or pass the project id as the argument", ce.Message)
	assert.Empty(t, log.all(), "refused before any lookup or request")
}

// TestCreateProjectEmptyIDIsRefused asserts an empty id that was typed is
// refused before any lookup or request, and is not read as no id.
func TestCreateProjectEmptyIDIsRefused(t *testing.T) {
	t.Parallel()
	cmd, _, log := generatingFixture(t)

	_, _, err := planProject(t, cmd, in{s: map[string]string{"cluster": "c-x", "display-name": "Web tier"}, a: []string{""}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, 2, ce.ExitCode())
	assert.Equal(t, "the project id is empty; pass an id, or leave it out and pass --display-name", ce.Message)
	assert.Empty(t, log.all())
}

const invalidProjectIDMessage = " is not valid: use lowercase letters, digits and '-', starting and ending with a letter or digit, at most 63 characters; to let Rancher generate the id, leave it out and pass --display-name"

// TestCreateProjectInvalidIDIsRefusedBeforeAnyRequest asserts an id that is
// not a DNS-1123 label is refused with the whole message, exit code 2 and
// no lookup or request. The row "-web" is a unit row: the command line
// parser reads it as a flag.
func TestCreateProjectInvalidIDIsRefusedBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, id, quoted string
	}{
		{"space and uppercase", "My Project", `'My Project'`},
		{"uppercase", "Web", `'Web'`},
		{"underscore", "web_tier", `'web_tier'`},
		{"leading hyphen", "-web", `'-web'`},
		{"trailing hyphen", "web-", `'web-'`},
		{"dot", "a.b", `'a.b'`},
		{"double dot", "a..b", `'a..b'`},
		{"64 characters", strings.Repeat("a", 64), "'" + strings.Repeat("a", 64) + "'"},
		{"253 characters", strings.Repeat("a", 253), "'" + strings.Repeat("a", 253) + "'"},
		{"single quote", "it's", `'it'\''s'`},
		{"zero-width character", "web\u200b", `'"web\u200b"'`},
		{"whitespace only", " ", `' '`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd, _, log := generatingFixture(t)
			_, _, err := planProject(t, cmd, in{s: map[string]string{"cluster": "c-x"}, a: []string{tc.id}})
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, 2, ce.ExitCode())
			assert.Equal(t, "project id "+tc.quoted+invalidProjectIDMessage, ce.Message)
			assert.Empty(t, ce.Hint, "no hint command")
			assert.Empty(t, log.all(), "refused before any lookup or request")
		})
	}
}

// TestCreateProjectValidIDReachesTheDryRun asserts each id that is a
// DNS-1123 label reaches the dry-run as metadata.name.
func TestCreateProjectValidIDReachesTheDryRun(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"web", "p-abc12", "a", "0", "a-b-c", strings.Repeat("a", 63)} {
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			cmd, client, _ := generatingFixture(t)
			_, _, err := planProject(t, cmd, in{s: map[string]string{"cluster": "c-x"}, a: []string{id}})
			require.NoError(t, err)
			require.NotNil(t, client.dryObj)
			assert.Equal(t, id, client.dryObj.GetName())
			_, hasGenerateName, _ := unstructured.NestedFieldNoCopy(client.dryObj.Object, "metadata", "generateName")
			assert.False(t, hasGenerateName, "an id means no generateName")
		})
	}
}

// TestCreateProjectDescribesItsIDAndDisplayName asserts the argument is
// optional and the command has a --display-name flag that is not
// declared required.
func TestCreateProjectDescribesItsIDAndDisplayName(t *testing.T) {
	t.Parallel()
	d := NewCreate(stubFactory{}).Describe()
	require.Len(t, d.Args, 1)
	assert.Equal(t, "id", d.Args[0].Name)
	assert.False(t, d.Args[0].Required)
	i := slices.IndexFunc(d.Flags, func(a runtime.ArgSpec) bool { return a.Name == "display-name" })
	require.GreaterOrEqual(t, i, 0)
	assert.False(t, d.Flags[i].Required)
	assert.False(t, d.Flags[i].AllowEmpty)
}
