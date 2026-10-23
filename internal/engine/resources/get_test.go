package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// memClient serves canned objects and records the namespaces/names it is asked
// for, so routing tests can assert what reached the client. getErr, when set,
// is returned instead of the items-miss default, so tests can inject a
// pre-curated NOT_FOUND (simulating what the real dynamic client now produces).
type memClient struct {
	items        []unstructured.Unstructured
	getErr       error
	listNS       []string
	listSelector []string
	getNS        []string
	getName      []string

	// table is what ListTable and GetTable answer, or tableErr; the calls
	// they receive are recorded like List and Get's.
	table             engine.TableAnswer
	tableErr          error
	listTableNS       []string
	listTableSelector []string
	getTableNS        []string
	getTableName      []string
}

var _ engine.TableReader = (*memClient)(nil)

func (m *memClient) ListTable(_ context.Context, _ schema.GroupVersionResource, ns, selector string) (engine.TableAnswer, error) {
	m.listTableNS = append(m.listTableNS, ns)
	m.listTableSelector = append(m.listTableSelector, selector)
	return m.table, m.tableErr
}

func (m *memClient) GetTable(_ context.Context, _ schema.GroupVersionResource, ns, name string) (engine.TableAnswer, error) {
	m.getTableNS = append(m.getTableNS, ns)
	m.getTableName = append(m.getTableName, name)
	return m.table, m.tableErr
}

func (m *memClient) Get(_ context.Context, _ schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	m.getNS = append(m.getNS, ns)
	m.getName = append(m.getName, name)
	if m.getErr != nil {
		return nil, m.getErr
	}
	if len(m.items) == 0 {
		return nil, clierr.New(clierr.CodeNotFound, name+" not found")
	}
	return &m.items[0], nil
}
func (m *memClient) List(_ context.Context, _ schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error) {
	m.listNS = append(m.listNS, ns)
	m.listSelector = append(m.listSelector, selector)
	return &unstructured.UnstructuredList{Items: m.items}, nil
}
func (m *memClient) Logs(context.Context, string, string, engine.LogOptions) (string, error) {
	return "", nil
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
	resolveErr   map[string]error
	clusterCalls []string
}

func (f *recFactory) Cluster(_ context.Context, id string) (engine.ClusterClient, error) {
	f.clusterCalls = append(f.clusterCalls, id)
	return f.client, nil
}
func (f *recFactory) ResolveCluster(_ context.Context, s string) (string, error) {
	if s == "" || s == engine.LocalCluster {
		return engine.LocalCluster, nil
	}
	if err, ok := f.resolveErr[s]; ok {
		return "", err
	}
	if id, ok := f.resolve[s]; ok {
		return id, nil
	}
	return s, nil
}

type input struct {
	args  []string
	s     map[string]string
	b     map[string]bool
	isSet map[string]bool
}

func (i input) String(k string) string  { return i.s[k] }
func (i input) Bool(k string) bool      { return i.b[k] }
func (i input) Strings(string) []string { return nil }
func (i input) IsSet(k string) bool     { return i.isSet[k] }
func (i input) Args() []string          { return i.args }

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

func roleTemplateObj(name, context, display string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata":    map[string]any{"name": name},
		"context":     context,
		"displayName": display,
	}}
}

func projectObj(name, display, cluster string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"displayName": display, "clusterName": cluster},
	}}
}

// nodeDriverObj mirrors the served wire shape: NodeDriverSpec.Builtin is
// `json:"builtin,omitempty"` upstream, so a non-builtin driver carries NO
// "builtin" key at all; Active is `json:"active"` (no omitempty) and is
// always present.
func nodeDriverObj(name, display string, active, builtin bool) unstructured.Unstructured {
	spec := map[string]any{"displayName": display, "active": active}
	if builtin {
		spec["builtin"] = builtin
	}
	return unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     spec,
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
	res, err := rc.Execute(t.Context(), in, nil)
	require.NoError(t, err)
	return res
}

// clusterObjReady builds a cluster fixture with spec.displayName and a single
// Ready condition. It delegates the conditions shape to conditionObj so the two
// test builders share a single source for status.conditions construction.
func clusterObjReady(name, display string, condStatus string) unstructured.Unstructured {
	base := conditionObj("Ready", condStatus)
	base.Object["metadata"] = map[string]any{"name": name}
	base.Object["spec"] = map[string]any{"displayName": display}
	return *base
}

func TestGetClusterListsWithColumns(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{
		clusterObjReady("local", "local", "True"),
		clusterObjReady("c-m-abc", "prod", "True"),
	}}
	cmd := NewGet(mustKind(t, "cluster"), &recFactory{client: client})

	d := cmd.Describe()
	assert.Equal(t, "get cluster", d.Name)
	assert.Equal(t, runtime.ReadOnly, d.CommandType)

	res := execGet(t, cmd, input{})
	assert.Equal(t, "ClusterList", res.Kind)
	tab, ok := res.Data.(output.Tabular)
	require.True(t, ok)
	assert.Equal(t, []string{"NAME", "DISPLAY NAME", "READY", "AGE"}, tab.Columns())
	require.Len(t, tab.Rows(), 2)
	assert.Equal(t, []string{"c-m-abc", "prod", "Ready", ""}, tab.Rows()[1])
}

func TestGetRoleTemplateListsWithColumns(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{
		roleTemplateObj("project-owner", "project", "Project Owner"),
		roleTemplateObj("cluster-owner", "cluster", "Cluster Owner"),
	}}
	cmd := NewGet(mustKind(t, "roletemplate"), &recFactory{client: client})

	res := execGet(t, cmd, input{})
	assert.Equal(t, "RoleTemplateList", res.Kind)
	tab, ok := res.Data.(output.Tabular)
	require.True(t, ok)
	assert.Equal(t, []string{"NAME", "CONTEXT", "DISPLAY NAME", "AGE"}, tab.Columns())
	require.Len(t, tab.Rows(), 2)
	assert.Equal(t, []string{"cluster-owner", "cluster", "Cluster Owner", ""}, tab.Rows()[1])
}

// TestGetNodeDriverListsWithColumns asserts the nodedriver row's read surface:
// the served v3 NodeDriver carries spec.displayName (string) and spec.active/
// spec.builtin (bools - rendered through the table renderer's bool fallback).
// The non-builtin driver's JSON omits spec.builtin entirely (omitempty on the
// wire) and its BUILTIN cell must still render "false", not blank.
func TestGetNodeDriverListsWithColumns(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{
		nodeDriverObj("amazonec2", "Amazon EC2", true, true),
		nodeDriverObj("custom-driver", "My Driver", false, false),
	}}
	cmd := NewGet(mustKind(t, "nodedriver"), &recFactory{client: client})

	d := cmd.Describe()
	assert.Equal(t, "get nodedriver", d.Name)
	assert.Equal(t, []string{"nodedrivers"}, d.Aliases)

	res := execGet(t, cmd, input{})
	assert.Equal(t, "NodeDriverList", res.Kind)
	tab, ok := res.Data.(output.Tabular)
	require.True(t, ok)
	assert.Equal(t, []string{"NAME", "DISPLAY NAME", "ACTIVE", "BUILTIN", "AGE"}, tab.Columns())
	require.Len(t, tab.Rows(), 2)
	assert.Equal(t, []string{"amazonec2", "Amazon EC2", "true", "true", ""}, tab.Rows()[0])
	assert.Equal(t, []string{"custom-driver", "My Driver", "false", "false", ""}, tab.Rows()[1])
}

// TestGetNodeDriverSingle asserts the named get: nodedrivers are cluster-scoped
// (NSNone) management resources, so the Get goes to the local cluster with no
// namespace.
func TestGetNodeDriverSingle(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{nodeDriverObj("amazonec2", "Amazon EC2", true, true)}}
	fac := &recFactory{client: client}
	res := execGet(t, NewGet(mustKind(t, "nodedriver"), fac), input{args: []string{"amazonec2"}})
	assert.Equal(t, "NodeDriver", res.Kind)
	assert.Equal(t, []string{"amazonec2"}, client.getName)
	assert.Equal(t, []string{""}, client.getNS, "nodedrivers are cluster-scoped")
	assert.Equal(t, []string{engine.LocalCluster}, fac.clusterCalls, "nodedrivers are served by the management cluster")
}

func TestGetClusterSingle(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{clusterObj("c-m-abc", "prod", "active")}}
	res := execGet(t, NewGet(mustKind(t, "cluster"), &recFactory{client: client}), input{args: []string{"prod"}})
	assert.Equal(t, "Cluster", res.Kind)
	assert.Equal(t, []string{"prod"}, client.getName)
	assert.Equal(t, []string{""}, client.getNS, "clusters are cluster-scoped")
}

func TestGetClusterNamedByID(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{clusterObj("c-m-abc", "prod", "active")}}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-m-abc"}}
	res := execGet(t, NewGet(mustKind(t, "cluster"), fac), input{args: []string{"c-m-abc"}})
	assert.Equal(t, "Cluster", res.Kind)
	assert.Equal(t, []string{"c-m-abc"}, client.getName, "an id argument passes through ResolveCluster unchanged")
}

func TestGetClusterNamedByDisplayName(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{clusterObj("c-m-abc", "prod", "active")}}
	fac := &recFactory{client: client, resolve: map[string]string{"prod": "c-m-abc"}}
	res := execGet(t, NewGet(mustKind(t, "cluster"), fac), input{args: []string{"prod"}})
	assert.Equal(t, "Cluster", res.Kind)
	assert.Equal(t, []string{"c-m-abc"}, client.getName, "the display name resolves to the cluster id before Get")
}

func TestGetClusterNamedAmbiguousDisplayName(t *testing.T) {
	t.Parallel()
	client := &memClient{}
	fac := &recFactory{client: client, resolveErr: map[string]error{
		"prod": clierr.New(clierr.CodeAmbiguousSelection, "2 clusters have display name \"prod\"").
			WithCandidates([]clierr.Candidate{{ID: "c-m-abc", Name: "prod"}, {ID: "c-m-def", Name: "prod"}}),
	}}
	_, err := NewGet(mustKind(t, "cluster"), fac).(runtime.ReadCommand).Execute(t.Context(), input{args: []string{"prod"}}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Empty(t, client.getName, "Get must not be called on an ambiguous resolution")
}

func TestGetClusterNamedMiss(t *testing.T) {
	t.Parallel()
	client := &memClient{}
	fac := &recFactory{client: client, resolveErr: map[string]error{
		"missing": clierr.New(clierr.CodeNotFound, "cluster not found: \"missing\"").
			WithCandidates([]clierr.Candidate{{ID: "local", Name: "local"}, {ID: "c-m-abc", Name: "prod"}}),
	}}
	_, err := NewGet(mustKind(t, "cluster"), fac).(runtime.ReadCommand).Execute(t.Context(), input{args: []string{"missing"}}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Empty(t, client.getName, "Get must not be called on a failed resolution")
}

// TestResultKindUsesK8sKindCasing asserts that envelope
// kinds echo the manifest's Kubernetes casing (k.K8sKind), not
// a naive first-rune capitalization, which would mangle
// multi-word kinds like "roletemplate" -> "Roletemplate" instead of
// "RoleTemplate".
func TestResultKindUsesK8sKindCasing(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "RoleTemplate", resultKind(mustKind(t, "roletemplate")))
	assert.Equal(t, "StatefulSet", resultKind(mustKind(t, "statefulset")))
	assert.Equal(t, "Pod", resultKind(mustKind(t, "pod")), "a single-word kind is unaffected")
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

// TestGetDescribeInheritsCapability asserts the requirement-inheritance rule: a
// kind row's Capability flows unchanged into the generated get command's
// Descriptor - one source, never a separately authored value.
func TestGetDescribeInheritsCapability(t *testing.T) {
	t.Parallel()
	req := runtime.CapabilityRequirement{Tier: runtime.CapabilityPrime, Feature: "widget-feature"}
	k := kinds.Kind{Name: "widget", Aliases: []string{"widgets"}, Capability: req}
	d := NewGet(k, nil).Describe()
	assert.Equal(t, req, d.Capability)
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

func TestGetDownstreamNamespacedNamedRequiresNamespace(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "pod")

	// get pod web --cluster prod, no --namespace → VALIDATION (named op).
	byName := NewGet(k, &recFactory{client: &memClient{items: []unstructured.Unstructured{podObj("web")}}, resolve: map[string]string{"prod": "c-abc"}})
	rc := byName.(runtime.ReadCommand)
	_, err := rc.Execute(t.Context(), input{args: []string{"web"}, s: map[string]string{"cluster": "prod"}}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce, "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	// get pods --cluster prod, no --namespace → lists across all namespaces.
	lc := &memClient{items: []unstructured.Unstructured{podObj("web")}}
	res := execGet(t, NewGet(k, &recFactory{client: lc, resolve: map[string]string{"prod": "c-abc"}}), input{s: map[string]string{"cluster": "prod"}})
	assert.Equal(t, "PodList", res.Kind)
	assert.Equal(t, []string{""}, lc.listNS, "a nameless list uses the all-namespaces query")
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

	// Get by name with an empty --cluster must NOT silently target "local".
	byName := NewGet(k, &recFactory{client: &memClient{items: []unstructured.Unstructured{projectObj("p-1", "web", "c-abc")}}})
	rc := byName.(runtime.ReadCommand)
	_, err := rc.Execute(t.Context(), input{args: []string{"web"}}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce, "want a clierr.Error, got %v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	// No name + empty --cluster lists across all clusters (namespace "").
	listAll := &memClient{items: []unstructured.Unstructured{projectObj("p-1", "web", "c-abc")}}
	fac2 := &recFactory{client: listAll}
	execGet(t, NewGet(k, fac2), input{})
	assert.Equal(t, []string{engine.LocalCluster}, fac2.clusterCalls)
	assert.Equal(t, []string{""}, listAll.listNS, "empty --cluster lists across all clusters")
}

// TestGetNamedNotFoundCurated asserts the curated NOT_FOUND shape for a named get:
// the dynamic client now produces "<kind> not found: <name>" with a list hint;
// the fake simulates that by returning a pre-curated error.
func TestGetNamedNotFoundCurated(t *testing.T) {
	t.Parallel()
	k := mustKind(t, "user")
	client := &memClient{getErr: kinds.NotFoundCurated(k, "u-xxx", "")}
	cmd := NewGet(k, &recFactory{client: client})
	_, err := cmd.(runtime.ReadCommand).Execute(t.Context(), input{args: []string{"u-xxx"}}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "user not found: u-xxx", ce.Message)
	assert.Equal(t, "list with: "+clierr.Rancher+" get users", ce.Hint)
}

// TestGetWaitNotFoundCurated asserts that the --wait path surfaces a curated
// NOT_FOUND from the dynamic client: the fake returns the pre-curated shape
// (as the real dynamic client now produces) and it propagates unchanged.
func TestGetWaitNotFoundCurated(t *testing.T) {
	k := mustKind(t, "cluster")
	client := &seqClient{
		objs: []*unstructured.Unstructured{nil},
		errs: []error{kinds.NotFoundCurated(k, "missing", "")},
	}
	cmd := NewGet(k, &recFactory{client: client}).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(), input{
		args: []string{"missing"},
		b:    map[string]bool{"wait": true},
	}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "cluster not found: missing", ce.Message)
	assert.Equal(t, "list with: "+clierr.Rancher+" get clusters", ce.Hint)
}

// TestGetNamedNotFoundWithHintPassesThrough asserts the has-hint guard: a
// NOT_FOUND that already carries a hint (e.g. resolveVersion's unsupported-
// group diagnostic) must pass through verbatim - its message and hint must
// not be overwritten by the generic "<kind> not found: <name>" curation.
func TestGetNamedNotFoundWithHintPassesThrough(t *testing.T) {
	t.Parallel()
	// Build an ext.cattle.io-style error: NOT_FOUND with a pre-existing hint.
	alreadyCurated := clierr.New(clierr.CodeNotFound,
		"this Rancher does not serve ext.cattle.io (requires Rancher >= 2.14, the supported minimum)").
		WithHint("token management: auth list-tokens; kubeconfig: the default exec-credential mode")
	client := &delClient{getErr: alreadyCurated}
	cmd := NewGet(mustKind(t, "token"), &recFactory{client: client})
	_, err := cmd.(runtime.ReadCommand).Execute(t.Context(), input{args: []string{"t-xxx"}}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Contains(t, ce.Message, "ext.cattle.io")
	assert.Contains(t, ce.Hint, "auth list-tokens", "already-curated hint must pass through untouched")
}

// TestGetNamedNonNotFoundPassesThrough asserts that a non-NOT_FOUND error from the
// client reaches the caller unchanged (code, message) without re-wrapping.
// Uses delClient (defined in delete_test.go) - the two stubs serve the same
// purpose; a fourth type would be the duplication.
func TestGetNamedNonNotFoundPassesThrough(t *testing.T) {
	t.Parallel()
	rbacErr := clierr.New(clierr.CodeRBAC, "forbidden")
	client := &delClient{getErr: rbacErr}
	cmd := NewGet(mustKind(t, "user"), &recFactory{client: client})
	_, err := cmd.(runtime.ReadCommand).Execute(t.Context(), input{args: []string{"u-xxx"}}, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, "forbidden", ce.Message)
}

// TestRoutePolicyMatrix asserts the routing contract for every NamespacePolicy:
// which cluster serves the client, which namespace reaches the API, and which
// inputs are VALIDATION errors. Route is shared by get, delete, and (branch 2)
// create/patch, so this matrix is the single behavior spec.
func TestRoutePolicyMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		kind        kinds.Kind
		flags       map[string]string
		op          Operation
		wantCluster string
		wantNS      string
		wantErrCode string // "" = success
	}{
		{name: "NSNone downstream routes to resolved cluster, no ns",
			kind:  kinds.Kind{Name: "node", Scope: kinds.Downstream, Policy: kinds.NSNone},
			flags: map[string]string{"cluster": "prod"}, op: OpNamed,
			wantCluster: "c-abc", wantNS: ""},
		{name: "NSNone management routes to local, no ns",
			kind:  kinds.Kind{Name: "cluster", Scope: kinds.Management, Policy: kinds.NSNone},
			flags: map[string]string{}, op: OpNamed,
			wantCluster: engine.LocalCluster, wantNS: ""},
		{name: "NSCluster named op requires cluster",
			kind:  kinds.Kind{Name: "project", Scope: kinds.Management, Policy: kinds.NSCluster},
			flags: map[string]string{}, op: OpNamed,
			wantErrCode: clierr.CodeValidation},
		{name: "NSCluster resolves cluster into namespace, client is management",
			kind:  kinds.Kind{Name: "project", Scope: kinds.Management, Policy: kinds.NSCluster},
			flags: map[string]string{"cluster": "prod"}, op: OpNamed,
			wantCluster: engine.LocalCluster, wantNS: "c-abc"},
		{name: "NSCluster list may omit cluster",
			kind:  kinds.Kind{Name: "project", Scope: kinds.Management, Policy: kinds.NSCluster},
			flags: map[string]string{}, op: OpList,
			wantCluster: engine.LocalCluster, wantNS: ""},
		{name: "NSWorkspace defaults namespace, management client, no cluster flag consulted",
			kind:  kinds.Kind{Name: "gitrepo", Scope: kinds.Management, Policy: kinds.NSWorkspace, DefaultNamespace: "fleet-default"},
			flags: map[string]string{}, op: OpNamed,
			wantCluster: engine.LocalCluster, wantNS: "fleet-default"},
		{name: "NSWorkspace namespace override",
			kind:  kinds.Kind{Name: "gitrepo", Scope: kinds.Management, Policy: kinds.NSWorkspace, DefaultNamespace: "fleet-default"},
			flags: map[string]string{"namespace": "team-a"}, op: OpNamed,
			wantCluster: engine.LocalCluster, wantNS: "team-a"},
		{name: "NSPlain named op requires namespace",
			kind:  kinds.Kind{Name: "pod", Scope: kinds.Downstream, Policy: kinds.NSPlain},
			flags: map[string]string{"cluster": "prod"}, op: OpNamed,
			wantErrCode: clierr.CodeValidation},
		{name: "NSPlain list may omit namespace",
			kind:  kinds.Kind{Name: "pod", Scope: kinds.Downstream, Policy: kinds.NSPlain},
			flags: map[string]string{"cluster": "prod"}, op: OpList,
			wantCluster: "c-abc", wantNS: ""},
		{name: "NSPlain management client routes to local (projectroletemplatebinding)",
			kind:  kinds.Kind{Name: "projectroletemplatebinding", Scope: kinds.Management, Policy: kinds.NSPlain},
			flags: map[string]string{"namespace": "p-1"}, op: OpNamed,
			wantCluster: engine.LocalCluster, wantNS: "p-1"},
		{name: "NSCluster kind rejects a project scope",
			kind:  kinds.Kind{Name: "project", Scope: kinds.Management, Policy: kinds.NSCluster},
			flags: map[string]string{"cluster": "prod", "project": "p-1"}, op: OpList,
			wantErrCode: clierr.CodeValidation},
		{name: "NSWorkspace kind rejects a project scope",
			kind:  kinds.Kind{Name: "gitrepo", Scope: kinds.Management, Policy: kinds.NSWorkspace, DefaultNamespace: "fleet-default"},
			flags: map[string]string{"project": "p-1"}, op: OpNamed,
			wantErrCode: clierr.CodeValidation},
		{name: "Management-scope NSPlain kind rejects a project scope",
			kind:  kinds.Kind{Name: "projectroletemplatebinding", Scope: kinds.Management, Policy: kinds.NSPlain},
			flags: map[string]string{"namespace": "p-1", "project": "p-1"}, op: OpNamed,
			wantErrCode: clierr.CodeValidation},
		{name: "Management-scope NSNone kind rejects a project scope",
			kind:  kinds.Kind{Name: "cluster", Scope: kinds.Management, Policy: kinds.NSNone},
			flags: map[string]string{"project": "p-1"}, op: OpNamed,
			wantErrCode: clierr.CodeValidation},
		{name: "Downstream NSNone kind (other than namespace) rejects a project scope",
			kind:  kinds.Kind{Name: "node", Scope: kinds.Downstream, Policy: kinds.NSNone},
			flags: map[string]string{"cluster": "prod", "project": "p-1"}, op: OpNamed,
			wantErrCode: clierr.CodeValidation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fac := &recFactory{resolve: map[string]string{"prod": "c-abc"}}
			ctx := t.Context()
			if tt.flags["project"] != "" {
				ctx = unreadProjectContext(t)
			}
			gotCluster, gotNS, _, _, err := Route(ctx, fac, tt.kind, input{s: tt.flags}, tt.op, "obj")
			if tt.wantErrCode != "" {
				var ce *clierr.Error
				require.ErrorAs(t, err, &ce, "want a clierr.Error, got %v", err)
				assert.Equal(t, tt.wantErrCode, ce.Code)
				if tt.flags["project"] != "" {
					// The refusal is ProjectRefusal's for the kind, the
					// rule the pipeline's command checks share.
					want := ProjectRefusal(tt.kind, tt.op, "obj", tt.flags["cluster"], tt.flags["project"])
					require.Error(t, want, "ProjectRefusal must refuse --project for %s", tt.kind.Name)
					assert.Equal(t, "--project does not apply to "+tt.kind.Name, ce.Message)
					assert.Equal(t, want.Error(), err.Error())
					assert.Empty(t, fac.clusterCalls, "the refusal sends no request")
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantCluster, gotCluster)
			assert.Equal(t, tt.wantNS, gotNS)
		})
	}
}

// TestRouteIgnoresUntypedProjectOnNonScopableKinds asserts that
// when no --project flag is typed, non-scopable kinds must not produce a
// "--project does not apply" rejection. The typed-flag rejections stay
// asserted in TestRoutePolicyMatrix above.
func TestRouteIgnoresUntypedProjectOnNonScopableKinds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		kind  kinds.Kind
		flags map[string]string
		op    Operation
	}{
		{name: "NSCluster (project itself)",
			kind:  kinds.Kind{Name: "project", Scope: kinds.Management, Policy: kinds.NSCluster},
			flags: map[string]string{"cluster": "prod"}, op: OpNamed},
		{name: "NSWorkspace (gitrepo)",
			kind:  kinds.Kind{Name: "gitrepo", Scope: kinds.Management, Policy: kinds.NSWorkspace, DefaultNamespace: "fleet-default"},
			flags: map[string]string{}, op: OpNamed},
		{name: "Management NSPlain (projectroletemplatebinding)",
			kind:  kinds.Kind{Name: "projectroletemplatebinding", Scope: kinds.Management, Policy: kinds.NSPlain},
			flags: map[string]string{"namespace": "p-1"}, op: OpNamed},
		{name: "Management NSNone (cluster)",
			kind:  kinds.Kind{Name: "cluster", Scope: kinds.Management, Policy: kinds.NSNone},
			flags: map[string]string{}, op: OpNamed},
		{name: "Downstream NSNone (node)",
			kind:  kinds.Kind{Name: "node", Scope: kinds.Downstream, Policy: kinds.NSNone},
			flags: map[string]string{"cluster": "prod"}, op: OpNamed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fac := &recFactory{resolve: map[string]string{"prod": "c-abc"}}
			_, _, _, _, err := Route(t.Context(), fac, tt.kind, input{s: tt.flags}, tt.op, "obj")
			require.NoError(t, err, "no --project flag must not reject a non-scopable kind")
		})
	}
}

// decodeEnvelope decodes a rendered envelope for the given format (json or
// yaml - sigs.k8s.io/yaml marshals through JSON, so both round-trip through
// json.Unmarshal-compatible structures) into v. Tests must decode and assert
// fields, never string-match the rendered document.
func decodeEnvelope(t *testing.T, format string, b []byte, v any) {
	t.Helper()
	if format == "yaml" {
		require.NoError(t, yaml.Unmarshal(b, v))
		return
	}
	require.NoError(t, json.Unmarshal(b, v))
}

// TestGetListRendersRealObjectsThroughOutputRender drives a list `get`
// through the actual runtime.Run → output.Render pipeline (not just
// Execute), because that's where the table projection used to swallow the
// data: table has no exported fields, so json.Marshal/yaml.Marshal on the
// envelope produced "data": {} for every kind's -o json/-o yaml. table now
// implements json.Marshaler to emit a bare array of the real objects.
func TestGetListRendersRealObjectsThroughOutputRender(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			client := &memClient{items: []unstructured.Unstructured{
				clusterObj("local", "local", "active"),
				clusterObj("c-m-abc", "prod", "active"),
			}}
			cmd := NewGet(mustKind(t, "cluster"), &recFactory{client: client})

			var buf bytes.Buffer
			_, ce := runtime.Run(t.Context(), cmd, input{}, runtime.IO{Out: &buf, Format: format}, safety.Binding{})
			require.Nil(t, ce)

			var env struct {
				Data []struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
				} `json:"data"`
			}
			decodeEnvelope(t, format, buf.Bytes(), &env)
			require.Len(t, env.Data, 2, "list must render as a bare array of the real objects, not an empty table projection")
			assert.Equal(t, "c-m-abc", env.Data[1].Metadata.Name)
		})
	}
}

// TestGetNamedRendersRealObjectThroughOutputRender is the named-get
// counterpart: a single object must render bare (never wrapped in
// {"items":[...]}), so gjson paths like data.metadata.name address it
// directly.
func TestGetNamedRendersRealObjectThroughOutputRender(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()
			client := &memClient{items: []unstructured.Unstructured{clusterObj("c-m-abc", "prod", "active")}}
			cmd := NewGet(mustKind(t, "cluster"), &recFactory{client: client})

			var buf bytes.Buffer
			_, ce := runtime.Run(t.Context(), cmd, input{args: []string{"prod"}}, runtime.IO{Out: &buf, Format: format}, safety.Binding{})
			require.Nil(t, ce)

			var env struct {
				Data struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
				} `json:"data"`
			}
			decodeEnvelope(t, format, buf.Bytes(), &env)
			assert.Equal(t, "c-m-abc", env.Data.Metadata.Name)
		})
	}
}

// TestPlural asserts kinds.Plural against every ending the kind table can
// produce, including the sibilant and hyphenated cases the naive name+"s"
// got wrong ("ingresss"). The function moved from resources to kinds so this
// test now calls the canonical copy there.
func TestPlural(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want string
	}{
		{"pod", "pods"},
		{"cluster", "clusters"},
		{"namespace", "namespaces"},
		{"event", "events"},
		{"ingress", "ingresses"},   // -s -> -es
		{"box", "boxes"},           // -x -> -es
		{"buzz", "buzzes"},         // -z -> -es
		{"watch", "watches"},       // -ch -> -es
		{"dish", "dishes"},         // -sh -> -es
		{"policy", "policies"},     // consonant + y -> -ies
		{"registry", "registries"}, // consonant + y -> -ies
		{"gateway", "gateways"},    // vowel + y -> +s, not -ies
		{"day", "days"},            // vowel + y -> +s
		{"fleet-cluster", "fleet-clusters"},
		{"provisioning-cluster", "provisioning-clusters"},
		{"amazonec2config", "amazonec2configs"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, c.want, kinds.Plural(c.name))
		})
	}
}

// newGetFixture builds a named-get command + input for a single ConfigMap.
// ConfigMap is NSPlain+Downstream, so a named get requires --namespace and --cluster.
func newGetFixture(t *testing.T, obj *unstructured.Unstructured) (runtime.Command, runtime.Input) {
	t.Helper()
	client := &memClient{items: []unstructured.Unstructured{*obj}}
	cmd := NewGet(mustKind(t, "configmap"), &recFactory{client: client})
	in := input{args: []string{"mf-cm"}, s: map[string]string{"namespace": "default", "cluster": "local"}}
	return cmd, in
}

// newGetListFixture builds a list-get command + input for ConfigMaps.
func newGetListFixture(t *testing.T, obj *unstructured.Unstructured) (runtime.Command, runtime.Input) {
	t.Helper()
	client := &memClient{items: []unstructured.Unstructured{*obj}}
	cmd := NewGet(mustKind(t, "configmap"), &recFactory{client: client})
	return cmd, input{s: map[string]string{"cluster": "local"}}
}

// decodeEnvelopeDyn decodes a rendered envelope into a map[string]any so
// tests can traverse the live object graph without a concrete struct target.
func decodeEnvelopeDyn(t *testing.T, b []byte, format string) map[string]any {
	t.Helper()
	var doc map[string]any
	if format == "yaml" {
		require.NoError(t, yaml.Unmarshal(b, &doc))
	} else {
		require.NoError(t, json.Unmarshal(b, &doc))
	}
	return doc
}

// TestGetOutputOmitsManagedFields drives named-get and list-get through the
// full runtime.Run → output.Render pipeline with a fixture that carries
// managedFields, and asserts the field is absent from both json and yaml
// output while neighboring metadata survives.
func TestGetOutputOmitsManagedFields(t *testing.T) {
	t.Parallel()
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "mf-cm",
			"namespace": "default",
			"uid":       "cm-uid-1",
			"managedFields": []any{
				map[string]any{"manager": "k3s", "operation": "Update"},
			},
			"ownerReferences": []any{map[string]any{"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": "own", "uid": "own-1", "controller": true}},
		},
		"data": map[string]any{"foo": "bar"},
	}}
	for _, format := range []string{"json", "yaml"} {
		t.Run("named-"+format, func(t *testing.T) {
			t.Parallel()
			cmd, in := newGetFixture(t, obj)
			var buf bytes.Buffer
			_, ce := runtime.Run(t.Context(), cmd, in,
				runtime.IO{Out: &buf, Format: format}, safety.Binding{})
			require.Nil(t, ce)
			doc := decodeEnvelopeDyn(t, buf.Bytes(), format)
			meta := doc["data"].(map[string]any)["metadata"].(map[string]any)
			assert.NotContains(t, meta, "managedFields")
			assert.Equal(t, "mf-cm", meta["name"])
			assert.Equal(t, "cm-uid-1", meta["uid"], "neighboring metadata survives")
			ownerRefs := meta["ownerReferences"].([]any)
			require.Len(t, ownerRefs, 1)
			assert.Equal(t, "own-1", ownerRefs[0].(map[string]any)["uid"])
		})
		t.Run("list-"+format, func(t *testing.T) {
			t.Parallel()
			cmd, in := newGetListFixture(t, obj)
			var buf bytes.Buffer
			_, ce := runtime.Run(t.Context(), cmd, in,
				runtime.IO{Out: &buf, Format: format}, safety.Binding{})
			require.Nil(t, ce)
			doc := decodeEnvelopeDyn(t, buf.Bytes(), format)
			items := doc["data"].([]any)
			require.Len(t, items, 1)
			meta := items[0].(map[string]any)["metadata"].(map[string]any)
			assert.NotContains(t, meta, "managedFields")
			assert.Equal(t, "cm-uid-1", meta["uid"])
		})
	}
}

// TestGetWaitStampsSummary proves that a successful --wait stamps res.Summary
// with the "wait: <expr> satisfied after <duration>" triage line.
func TestGetWaitStampsSummary(t *testing.T) {
	withFastPoll(t)
	synctest.Test(t, func(t *testing.T) {
		k := mustKind(t, "cluster")
		client := &seqClient{objs: []*unstructured.Unstructured{
			conditionObj("Ready", "False"),
			conditionObj("Ready", "True"),
		}}
		cmd := NewGet(k, &recFactory{client: client}).(runtime.ReadCommand)
		res, err := cmd.Execute(t.Context(), input{
			args: []string{"c-abc"},
			b:    map[string]bool{"wait": true},
		}, nil)
		require.NoError(t, err)
		const wantPrefix = "wait: condition=Ready satisfied after "
		assert.True(t, strings.HasPrefix(res.Summary, wantPrefix),
			"want summary prefix %q, got %q", wantPrefix, res.Summary)
	})
}

// TestGetWaitFreshSuccessWarning covers the Execute-level fresh-success wiring:
// a WaitFailsOnStalled kind (gitrepo) whose wait resolves immediately against a
// fresh, settled, satisfied object must carry the advisory warning on
// Result.Warnings; a non-WaitFailsOnStalled kind (pod) under the same
// conditions must carry no warnings (the fresh-success gate is opt-in per kind).
func TestGetWaitFreshSuccessWarning(t *testing.T) {
	withFastPoll(t)

	// freshReadyObj returns a settled (no observedGeneration), freshly created
	// (creationTimestamp=now), satisfied (Ready=True) unstructured object.
	// settled() returns true when observedGeneration is absent, so no explicit
	// generation bookkeeping is needed; the fresh creationTimestamp triggers
	// freshSuccessWarning when WaitFailsOnStalled is true.
	freshReadyObj := func() *unstructured.Unstructured {
		obj := conditionObj("Ready", "True")
		obj.SetCreationTimestamp(metav1.Now())
		return obj
	}

	t.Run("WaitFailsOnStalled kind carries fresh-success advisory", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			k := mustKind(t, "gitrepo")
			client := &seqClient{objs: []*unstructured.Unstructured{freshReadyObj()}}
			cmd := NewGet(k, &recFactory{client: client}).(runtime.ReadCommand)
			res, err := cmd.Execute(t.Context(), input{
				args: []string{"my-repo"},
				b:    map[string]bool{"wait": true},
			}, nil)
			require.NoError(t, err)
			require.Len(t, res.Warnings, 1, "one fresh-success advisory expected")
			assert.Contains(t, res.Warnings[0], "gitrepo")
			assert.Contains(t, res.Warnings[0], "my-repo")
			assert.Contains(t, res.Warnings[0], "fleet-default", "re-check hint must name the resolved namespace")
		})
	})

	t.Run("non-WaitFailsOnStalled kind carries no warnings", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			k := mustKind(t, "pod")
			client := &seqClient{objs: []*unstructured.Unstructured{freshReadyObj()}}
			cmd := NewGet(k, &recFactory{client: client}).(runtime.ReadCommand)
			res, err := cmd.Execute(t.Context(), input{
				args: []string{"my-pod"},
				b:    map[string]bool{"wait": true},
				s:    map[string]string{"namespace": "default", "for": "condition=Ready", "cluster": "local"},
			}, nil)
			require.NoError(t, err)
			assert.Empty(t, res.Warnings, "pod has WaitFailsOnStalled=false; no fresh-success advisory expected")
		})
	})

	t.Run("WaitFailsOnStalled kind waiting for Stalled carries no advisory", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// A satisfied Stalled=True wait is the controller's real verdict, not a
			// stale birth default - the freshSuccessWarning must be suppressed when
			// the wait target is condition=Stalled.
			freshStalledObj := func() *unstructured.Unstructured {
				obj := conditionObj("Stalled", "True")
				obj.SetCreationTimestamp(metav1.Now())
				return obj
			}
			k := mustKind(t, "gitrepo")
			client := &seqClient{objs: []*unstructured.Unstructured{freshStalledObj()}}
			cmd := NewGet(k, &recFactory{client: client}).(runtime.ReadCommand)
			res, err := cmd.Execute(t.Context(), input{
				args: []string{"my-repo"},
				b:    map[string]bool{"wait": true},
				s:    map[string]string{"for": "condition=Stalled=True"},
			}, nil)
			require.NoError(t, err)
			assert.Empty(t, res.Warnings, "condition=Stalled wait satisfied immediately must carry no fresh-success advisory")
		})
	})
}

// TestGetEnrichersAttachWarnings asserts NewGet's variadic GetEnricher hook: it
// runs against the named object on a named get, and against every item on a
// list get - never against items an enricher does not match.
func TestGetEnrichersAttachWarnings(t *testing.T) {
	t.Parallel()
	warnPromoted := func(_ context.Context, obj *unstructured.Unstructured) (string, bool) {
		name, _, _ := unstructured.NestedString(obj.Object, "metadata", "name")
		if name == "c-m-abc" {
			return "warned: " + name, true
		}
		return "", false
	}

	t.Run("named", func(t *testing.T) {
		t.Parallel()
		client := &memClient{items: []unstructured.Unstructured{clusterObj("c-m-abc", "prod", "active")}}
		res := execGet(t, NewGet(mustKind(t, "cluster"), &recFactory{client: client}, warnPromoted), input{args: []string{"prod"}})
		assert.Equal(t, []string{"warned: c-m-abc"}, res.Warnings)
	})

	t.Run("list: only the matching item warns", func(t *testing.T) {
		t.Parallel()
		client := &memClient{items: []unstructured.Unstructured{
			clusterObj("c-m-abc", "prod", "active"),
			clusterObj("c-m-def", "staging", "active"),
		}}
		res := execGet(t, NewGet(mustKind(t, "cluster"), &recFactory{client: client}, warnPromoted), input{})
		assert.Equal(t, []string{"warned: c-m-abc"}, res.Warnings)
	})

	t.Run("no enrichers: no warnings", func(t *testing.T) {
		t.Parallel()
		client := &memClient{items: []unstructured.Unstructured{clusterObj("c-m-abc", "prod", "active")}}
		res := execGet(t, NewGet(mustKind(t, "cluster"), &recFactory{client: client}), input{args: []string{"prod"}})
		assert.Empty(t, res.Warnings)
	})
}

// TestWaitElapsed asserts the boundary behaviour of waitElapsed: the
// millisecond floor for sub-second waits and the cutover to the
// apimachinery short form at exactly one second.
func TestWaitElapsed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		elapsed time.Duration
		want    string
		comment string
	}{
		// Sub-second: floor to 1ms so a zero-elapsed wait never prints "0s".
		{0, "1ms", "zero duration floors to 1ms"},
		{400 * time.Microsecond, "1ms", "400µs rounds down to 0ms, then floors to 1ms"},
		{499 * time.Microsecond, "1ms", "499µs rounds down to 0ms, then floors to 1ms"},
		// Round is half-away-from-zero: 500µs rounds to 1ms, no floor needed.
		{500 * time.Microsecond, "1ms", "500µs rounds up to 1ms (half-away-from-zero)"},
		{999 * time.Millisecond, "999ms", "999ms rounds to 999ms, stays below the cutover"},
		// At or above one second: Go's second-rounded format.
		{1 * time.Second, "1s", "exactly 1s → second-rounded 1s"},
		{90 * time.Second, "1m30s", "90s → second-rounded 1m30s"},
		{95 * time.Second, "1m35s", "95s → second-rounded 1m35s (the live case)"},
		{5 * time.Minute, "5m0s", "5m → second-rounded 5m0s"},
		// Hour-scale: full precision is deliberate (not the coarser AGE-column form).
		{2*time.Hour + 15*time.Minute + 30*time.Second, "2h15m30s", "2h15m30s stays at hour scale"},
		{1*time.Hour + 0*time.Minute + 1*time.Second, "1h0m1s", "1h0m1s - zero-minute field retained"},
	}
	for _, tt := range tests {
		t.Run(tt.comment, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, waitElapsed(tt.elapsed))
		})
	}
}

// userObj builds a user fixture whose principalIds is exactly the given
// slice - nil sets the field to a JSON null, not an absent key; use
// userObjNoPrincipals for a genuinely absent field.
func userObj(name string, principals []any) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata":     map[string]any{"name": name},
		"principalIds": principals,
	}}
}

func userObjNoPrincipals(name string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name}}}
}

func userObjNonArrayPrincipals(name string, val any) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"metadata":     map[string]any{"name": name},
		"principalIds": val,
	}}
}

// TestIsSystemManagedBoundaryFixtures asserts the per-element predicate exactly
// against Rancher's user-store prefix "system://" - the full boundary
// set: near-miss prefixes, non-string elements, mixed arrays (asserting the
// per-element fail-open cast), non-array fields, and an absent field.
func TestIsSystemManagedBoundaryFixtures(t *testing.T) {
	t.Parallel()
	f := &kinds.PrefixFilter{Path: []string{"principalIds"}, Prefix: "system://", Label: "system account"}
	tests := []struct {
		name string
		obj  unstructured.Unstructured
		want bool
	}{
		{"agent account", userObj("u-1", []any{"system://c-abc"}), true},
		{"provisioning account", userObj("u-2", []any{"system://provisioning/fleet-default/c-abc"}), true},
		{"real local user", userObj("u-3", []any{"local://u-3"}), false},
		{"provider user", userObj("u-4", []any{"okta_user://u-4"}), false},
		{"multiple principals, system not first", userObj("u-5", []any{"local://u-5", "system://c-abc"}), true},
		{"empty principalIds", userObj("u-6", []any{}), false},
		{"absent field", userObjNoPrincipals("u-7"), false},
		{"nil principalIds", userObj("u-8", nil), false},
		{"near-miss: system:serviceaccount", userObj("u-9", []any{"system:serviceaccount:kube-system:default"}), false},
		{"near-miss: xsystem://", userObj("u-10", []any{"xsystem://c-abc"}), false},
		{"empty-string element", userObj("u-11", []any{""}), false},
		{"only non-string elements", userObj("u-12", []any{float64(42)}), false},
		{"mixed array: non-string then system", userObj("u-13", []any{float64(42), "system://c-abc"}), true},
		{"non-array field", userObjNonArrayPrincipals("u-14", "system://c-abc"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, f.Matches(tt.obj.Object))
		})
	}
}

func TestSystemHiddenWarningWording(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "1 system account hidden - pass --include-system to show it", systemHiddenWarning(1, "system account"))
	assert.Equal(t, "4 system accounts hidden - pass --include-system to show them", systemHiddenWarning(4, "system account"))
}

// TestGetUsersHidesSystemAccountsByDefault covers the LIST default: a system
// account is dropped before rendering and a singular warning is stamped;
// a real user survives.
func TestGetUsersHidesSystemAccountsByDefault(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{
		userObj("u-alice", []any{"local://u-alice"}),
		userObj("u-sys", []any{"system://local"}),
	}}
	cmd := NewGet(mustKind(t, "user"), &recFactory{client: client})
	res := execGet(t, cmd, input{})
	tbl, ok := res.Data.(table)
	require.True(t, ok)
	require.Len(t, tbl.items, 1)
	assert.Equal(t, "u-alice", tbl.items[0].GetName())
	require.Len(t, res.Warnings, 1)
	assert.Equal(t, "1 system account hidden - pass --include-system to show it", res.Warnings[0])
}

// TestGetUsersIncludeSystemShowsAll covers --include-system: every object is
// listed and no warning is stamped.
func TestGetUsersIncludeSystemShowsAll(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{
		userObj("u-alice", []any{"local://u-alice"}),
		userObj("u-sys", []any{"system://local"}),
	}}
	cmd := NewGet(mustKind(t, "user"), &recFactory{client: client})
	res := execGet(t, cmd, input{b: map[string]bool{flagIncludeSystem: true}})
	tbl, ok := res.Data.(table)
	require.True(t, ok)
	assert.Len(t, tbl.items, 2)
	assert.Empty(t, res.Warnings)
}

// TestGetUsersNoWarningWhenNothingHidden covers the absent-warning case: no
// system accounts present, no warning stamped even though the filter runs.
func TestGetUsersNoWarningWhenNothingHidden(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{userObj("u-alice", []any{"local://u-alice"})}}
	cmd := NewGet(mustKind(t, "user"), &recFactory{client: client})
	res := execGet(t, cmd, input{})
	tbl, ok := res.Data.(table)
	require.True(t, ok)
	assert.Len(t, tbl.items, 1)
	assert.Empty(t, res.Warnings)
}

// TestGetUserNamedSystemAccountSucceeds asserts the deliberate divergence from
// Norman: naming a system account is intent, so a named get returns it with
// no flag at all.
func TestGetUserNamedSystemAccountSucceeds(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{userObj("u-sys", []any{"system://local"})}}
	cmd := NewGet(mustKind(t, "user"), &recFactory{client: client})
	res := execGet(t, cmd, input{args: []string{"u-sys"}})
	tbl, ok := res.Data.(table)
	require.True(t, ok)
	require.Len(t, tbl.items, 1)
	assert.Equal(t, "u-sys", tbl.items[0].GetName())
}

// TestGetUserNamedIncludeSystemRejected asserts the typed no-op rejection: a
// named get with --include-system is a VALIDATION error naming the reason in
// its message, matching the repo's --project/--for typed no-op precedent.
func TestGetUserNamedIncludeSystemRejected(t *testing.T) {
	t.Parallel()
	cmd, ok := NewGet(mustKind(t, "user"), &recFactory{client: &memClient{}}).(runtime.ReadCommand)
	require.True(t, ok)
	_, err := cmd.Execute(t.Context(), input{
		args:  []string{"u-alice"},
		b:     map[string]bool{flagIncludeSystem: true},
		isSet: map[string]bool{flagIncludeSystem: true},
	}, nil)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "a named get always returns the object")
	assert.Empty(t, ce.Hint)
}

// TestGetUserNamedIncludeSystemFalseRejected asserts the IsSet seam: an
// explicit --include-system=false on a named get is still a typed no-op and
// must be rejected the same as --include-system=true (tri-state).
func TestGetUserNamedIncludeSystemFalseRejected(t *testing.T) {
	t.Parallel()
	cmd, ok := NewGet(mustKind(t, "user"), &recFactory{client: &memClient{}}).(runtime.ReadCommand)
	require.True(t, ok)
	_, err := cmd.Execute(t.Context(), input{
		args:  []string{"u-alice"},
		b:     map[string]bool{flagIncludeSystem: false},
		isSet: map[string]bool{flagIncludeSystem: true},
	}, nil)
	require.Error(t, err)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// TestGetIncludeSystemAbsentOnKindsWithoutSystemFilter asserts the
// no-silent-no-op rule: --include-system is not generated at all for a kind
// without SystemFilter, so a get pods Descriptor must not carry the flag.
func TestGetIncludeSystemAbsentOnKindsWithoutSystemFilter(t *testing.T) {
	t.Parallel()
	cmd := NewGet(mustKind(t, "pod"), &recFactory{})
	d := cmd.Describe()
	for _, f := range d.Flags {
		assert.NotEqual(t, flagIncludeSystem, f.Name, "get pod must not carry --include-system")
	}
}

// TestGetUsersDescribeCarriesIncludeSystemFlag asserts the generation site: a
// SystemFilter kind's Describe carries --include-system and a synthesized
// Desc documenting default hiding, the flag, and the named-get divergence.
func TestGetUsersDescribeCarriesIncludeSystemFlag(t *testing.T) {
	t.Parallel()
	cmd := NewGet(mustKind(t, "user"), &recFactory{})
	d := cmd.Describe()
	var found bool
	for _, f := range d.Flags {
		if f.Name == flagIncludeSystem {
			found = true
			assert.Equal(t, "bool", f.Type)
		}
	}
	assert.True(t, found, "get user must carry --include-system")
	assert.NotEmpty(t, d.Desc, "get user must synthesize a Desc documenting the SystemFilter behavior")
}

// TestGetSelectorReachesTheListRequest asserts --selector is passed to the
// server as the list's label selector, unchanged.
func TestGetSelectorReachesTheListRequest(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{clusterObj("local", "local", "active")}}
	cmd := NewGet(mustKind(t, "cluster"), &recFactory{client: client})
	_, err := cmd.(runtime.ReadCommand).Execute(t.Context(), input{s: map[string]string{"selector": "app=web,tier in (a,b)"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"app=web,tier in (a,b)"}, client.listSelector)
}

// TestGetSelectorJoinsTheProjectScopeSelector asserts a list that already
// carries the project scope's selector (the namespace kind under --project)
// sends both requirements as one conjunction.
func TestGetSelectorJoinsTheProjectScopeSelector(t *testing.T) {
	t.Parallel()
	client, fac := newRouteProjectFixture()
	ctx, _ := projectContext(t, "ns-a")
	cmd := NewGet(mustKind(t, "namespace"), fac)
	_, err := cmd.(runtime.ReadCommand).Execute(ctx,
		input{s: map[string]string{"cluster": "prod", "project": "web", "selector": "app=web"}}, nil)
	require.NoError(t, err)
	var got []string
	for _, call := range client.listCalls {
		if call.resource == "namespaces" {
			got = append(got, call.selector)
		}
	}
	assert.Equal(t, []string{"field.cattle.io/projectId=p-1,app=web"}, got)
}

// TestGetSelectorInvalidIsValidationBeforeAnyRequest asserts a selector
// the Kubernetes grammar rejects fails VALIDATION with the parser's reason
// and never reaches the server.
func TestGetSelectorInvalidIsValidationBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	client := &memClient{}
	cmd := NewGet(mustKind(t, "cluster"), &recFactory{client: client})
	_, err := cmd.(runtime.ReadCommand).Execute(t.Context(), input{s: map[string]string{"selector": "app in (web"}}, nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "--selector")
	assert.Empty(t, client.listSelector, "no list request for an invalid selector")
}

// TestGetSelectorWithNameIsValidation asserts a name and a selector cannot
// be combined: a named get returns the object, a selector filters a list.
func TestGetSelectorWithNameIsValidation(t *testing.T) {
	t.Parallel()
	client := &memClient{items: []unstructured.Unstructured{clusterObj("local", "local", "active")}}
	cmd := NewGet(mustKind(t, "cluster"), &recFactory{client: client})
	_, err := cmd.(runtime.ReadCommand).Execute(t.Context(),
		input{args: []string{"local"}, s: map[string]string{"cluster": "local", "selector": "app=web"}}, nil)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "--selector does not apply to a named cluster")
	assert.Empty(t, client.getName, "no request is made")
}

// TestGetSelectorIsDeclaredOnGetOnly asserts the flag is on every get
// descriptor with its kubectl short form, and on no delete or patch
// descriptor (they share getFlags, which must not carry it).
func TestGetSelectorIsDeclaredOnGetOnly(t *testing.T) {
	t.Parallel()
	for _, k := range kinds.All() {
		var found *runtime.ArgSpec
		for _, f := range NewGet(k, nil).Describe().Flags {
			if f.Name == "selector" {
				found = &f
			}
		}
		require.NotNil(t, found, "get %s must declare --selector", k.Name)
		assert.Equal(t, []string{"l"}, found.Aliases, "get %s", k.Name)
		assert.Equal(t, "string", found.Type)
		assert.False(t, found.Required)
		if k.Deletable {
			for _, f := range NewDelete(k, nil).Describe().Flags {
				assert.NotEqual(t, "selector", f.Name, "delete %s must not carry --selector", k.Name)
			}
		}
		for _, f := range NewPatch(k, nil, nil).Describe().Flags {
			assert.NotEqual(t, "selector", f.Name, "patch %s must not carry --selector", k.Name)
		}
	}
}

// TestGetSelectorReachesTheListUnderProjectMemberFilter asserts the other
// project-scoped shape: a downstream namespaced kind under --project without
// --namespace carries a client-side member set and no scope selector, so the
// list request carries --selector alone.
func TestGetSelectorReachesTheListUnderProjectMemberFilter(t *testing.T) {
	t.Parallel()
	client, fac := newRouteProjectFixture()
	ctx, _ := projectContext(t, "ns-a")
	cmd := NewGet(mustKind(t, "pod"), fac)
	_, err := cmd.(runtime.ReadCommand).Execute(ctx,
		input{s: map[string]string{"cluster": "prod", "project": "web", "selector": "app=web"}}, nil)
	require.NoError(t, err)
	var got []string
	for _, call := range client.listCalls {
		if call.resource == "pods" {
			got = append(got, call.selector)
		}
	}
	assert.Equal(t, []string{"app=web"}, got)
}

func (*memClient) PostReview(context.Context, engine.ReviewRequest) (engine.ReviewResponse, error) {
	return engine.ReviewResponse{}, errors.New("PostReview not scripted in this fake")
}

func (*memClient) ServerResources(context.Context) ([]engine.APIResource, error) {
	return nil, errors.New("ServerResources not scripted in this fake")
}

func (f *recFactory) ImpersonatedCluster(ctx context.Context, id string, _ engine.Impersonation) (engine.ClusterClient, error) {
	return f.Cluster(ctx, id)
}

// TestParseWaitTimeoutIsTheSharedDurationContract asserts get --wait's
// --timeout follows the one duration-flag contract: omitted is the 120s
// default, a positive duration is taken as written, and a zero, negative
// or unparsable value is a VALIDATION error showing this flag's example.
func TestParseWaitTimeoutIsTheSharedDurationContract(t *testing.T) {
	t.Parallel()
	d, err := parseWaitTimeout(input{})
	require.NoError(t, err)
	assert.Equal(t, defaultWaitTimeout, d)
	d, err = parseWaitTimeout(input{s: map[string]string{flagTimeout: "5m"}})
	require.NoError(t, err)
	assert.Equal(t, 5*time.Minute, d)
	for raw, want := range map[string]string{
		"soon": `--timeout "soon" is not a valid duration, e.g. 5m`,
		"0":    `--timeout "0" must be a positive duration, e.g. 5m`,
		"-1s":  `--timeout "-1s" must be a positive duration, e.g. 5m`,
	} {
		_, err := parseWaitTimeout(input{s: map[string]string{flagTimeout: raw}})
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, raw)
		assert.Equal(t, clierr.CodeValidation, ce.Code, raw)
		require.ErrorContains(t, err, want)
	}
}
