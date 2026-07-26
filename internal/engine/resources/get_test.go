package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
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
	if len(m.items) == 0 {
		return nil, clierr.New(clierr.CodeNotFound, name+" not found")
	}
	return &m.items[0], nil
}
func (m *memClient) List(_ context.Context, _ schema.GroupVersionResource, ns, _ string) (*unstructured.UnstructuredList, error) {
	m.listNS = append(m.listNS, ns)
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
	ctxCluster   string
	ctxProject   string
	clusterCalls []string
}

func (f *recFactory) Cluster(_ context.Context, id string) (engine.ClusterClient, error) {
	f.clusterCalls = append(f.clusterCalls, id)
	return f.client, nil
}
func (f *recFactory) ContextCluster() (string, error) { return f.ctxCluster, nil }
func (f *recFactory) ContextProject() (string, error) { return f.ctxProject, nil }
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
	args []string
	s    map[string]string
	b    map[string]bool
}

func (i input) String(k string) string  { return i.s[k] }
func (i input) Bool(k string) bool      { return i.b[k] }
func (i input) Strings(string) []string { return nil }
func (i input) IsSet(string) bool       { return false }
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
	assert.Equal(t, []string{"NAME", "CONTEXT", "DISPLAY"}, tab.Columns())
	require.Len(t, tab.Rows(), 2)
	assert.Equal(t, []string{"cluster-owner", "cluster", "Cluster Owner"}, tab.Rows()[1])
}

// TestGetNodeDriverListsWithColumns pins the nodedriver row's read surface:
// the served v3 NodeDriver carries spec.displayName (string) and spec.active/
// spec.builtin (bools — rendered through the table renderer's bool fallback).
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
	assert.Equal(t, []string{"NAME", "DISPLAYNAME", "ACTIVE", "BUILTIN"}, tab.Columns())
	require.Len(t, tab.Rows(), 2)
	assert.Equal(t, []string{"amazonec2", "Amazon EC2", "true", "true"}, tab.Rows()[0])
	assert.Equal(t, []string{"custom-driver", "My Driver", "false", "false"}, tab.Rows()[1])
}

// TestGetNodeDriverSingle pins the named get: nodedrivers are cluster-scoped
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
	_, err := NewGet(mustKind(t, "cluster"), fac).(runtime.ReadCommand).Execute(context.Background(), input{args: []string{"prod"}}, nil)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
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
	_, err := NewGet(mustKind(t, "cluster"), fac).(runtime.ReadCommand).Execute(context.Background(), input{args: []string{"missing"}}, nil)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce))
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Empty(t, client.getName, "Get must not be called on a failed resolution")
}

// TestResultKindUsesK8sKindCasing is the exercise bug 7 repro: envelope
// kinds must echo the manifest's Kubernetes casing (k.K8sKind), not
// kindTitle(k.Name)'s naive first-rune capitalization, which mangles
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
	_, err := rc.Execute(context.Background(), input{args: []string{"web"}, s: map[string]string{"cluster": "prod"}}, nil)
	var ce *clierr.Error
	require.True(t, errors.As(err, &ce), "want a clierr.Error, got %v", err)
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

// TestRoutePolicyMatrix pins the routing contract for every NamespacePolicy:
// which cluster serves the client, which namespace reaches the API, and which
// inputs are VALIDATION errors. Route is shared by get, delete, and (branch 2)
// create/patch, so this matrix is the single behavior spec.
func TestRoutePolicyMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		kind        kinds.Kind
		flags       map[string]string
		named       bool // operation targets a single name
		wantCluster string
		wantNS      string
		wantErrCode string // "" = success
	}{
		{name: "NSNone downstream routes to resolved cluster, no ns",
			kind:  kinds.Kind{Name: "node", Scope: kinds.Downstream, Policy: kinds.NSNone},
			flags: map[string]string{"cluster": "prod"}, named: true,
			wantCluster: "c-abc", wantNS: ""},
		{name: "NSNone management routes to local, no ns",
			kind:  kinds.Kind{Name: "cluster", Scope: kinds.Management, Policy: kinds.NSNone},
			flags: map[string]string{}, named: true,
			wantCluster: engine.LocalCluster, wantNS: ""},
		{name: "NSCluster named op requires cluster",
			kind:  kinds.Kind{Name: "project", Scope: kinds.Management, Policy: kinds.NSCluster},
			flags: map[string]string{}, named: true,
			wantErrCode: clierr.CodeValidation},
		{name: "NSCluster resolves cluster into namespace, client is management",
			kind:  kinds.Kind{Name: "project", Scope: kinds.Management, Policy: kinds.NSCluster},
			flags: map[string]string{"cluster": "prod"}, named: true,
			wantCluster: engine.LocalCluster, wantNS: "c-abc"},
		{name: "NSCluster list may omit cluster",
			kind:  kinds.Kind{Name: "project", Scope: kinds.Management, Policy: kinds.NSCluster},
			flags: map[string]string{}, named: false,
			wantCluster: engine.LocalCluster, wantNS: ""},
		{name: "NSWorkspace defaults namespace, management client, no cluster flag consulted",
			kind:  kinds.Kind{Name: "gitrepo", Scope: kinds.Management, Policy: kinds.NSWorkspace, DefaultNamespace: "fleet-default"},
			flags: map[string]string{}, named: true,
			wantCluster: engine.LocalCluster, wantNS: "fleet-default"},
		{name: "NSWorkspace namespace override",
			kind:  kinds.Kind{Name: "gitrepo", Scope: kinds.Management, Policy: kinds.NSWorkspace, DefaultNamespace: "fleet-default"},
			flags: map[string]string{"namespace": "team-a"}, named: true,
			wantCluster: engine.LocalCluster, wantNS: "team-a"},
		{name: "NSPlain named op requires namespace",
			kind:  kinds.Kind{Name: "pod", Scope: kinds.Downstream, Policy: kinds.NSPlain},
			flags: map[string]string{"cluster": "prod"}, named: true,
			wantErrCode: clierr.CodeValidation},
		{name: "NSPlain list may omit namespace",
			kind:  kinds.Kind{Name: "pod", Scope: kinds.Downstream, Policy: kinds.NSPlain},
			flags: map[string]string{"cluster": "prod"}, named: false,
			wantCluster: "c-abc", wantNS: ""},
		{name: "NSPlain management client routes to local (projectroletemplatebinding)",
			kind:  kinds.Kind{Name: "projectroletemplatebinding", Scope: kinds.Management, Policy: kinds.NSPlain},
			flags: map[string]string{"namespace": "p-1"}, named: true,
			wantCluster: engine.LocalCluster, wantNS: "p-1"},
		{name: "NSCluster kind rejects a project scope",
			kind:  kinds.Kind{Name: "project", Scope: kinds.Management, Policy: kinds.NSCluster},
			flags: map[string]string{"cluster": "prod", "project": "p-1"}, named: false,
			wantErrCode: clierr.CodeValidation},
		{name: "NSWorkspace kind rejects a project scope",
			kind:  kinds.Kind{Name: "gitrepo", Scope: kinds.Management, Policy: kinds.NSWorkspace, DefaultNamespace: "fleet-default"},
			flags: map[string]string{"project": "p-1"}, named: true,
			wantErrCode: clierr.CodeValidation},
		{name: "Management-scope NSPlain kind rejects a project scope",
			kind:  kinds.Kind{Name: "projectroletemplatebinding", Scope: kinds.Management, Policy: kinds.NSPlain},
			flags: map[string]string{"namespace": "p-1", "project": "p-1"}, named: true,
			wantErrCode: clierr.CodeValidation},
		{name: "Management-scope NSNone kind rejects a project scope",
			kind:  kinds.Kind{Name: "cluster", Scope: kinds.Management, Policy: kinds.NSNone},
			flags: map[string]string{"project": "p-1"}, named: true,
			wantErrCode: clierr.CodeValidation},
		{name: "Downstream NSNone kind (other than namespace) rejects a project scope",
			kind:  kinds.Kind{Name: "node", Scope: kinds.Downstream, Policy: kinds.NSNone},
			flags: map[string]string{"cluster": "prod", "project": "p-1"}, named: true,
			wantErrCode: clierr.CodeValidation},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fac := &recFactory{resolve: map[string]string{"prod": "c-abc"}}
			gotCluster, gotNS, _, _, err := Route(context.Background(), fac, tt.kind, input{s: tt.flags}, tt.named)
			if tt.wantErrCode != "" {
				var ce *clierr.Error
				require.True(t, errors.As(err, &ce), "want a clierr.Error, got %v", err)
				assert.Equal(t, tt.wantErrCode, ce.Code)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantCluster, gotCluster)
			assert.Equal(t, tt.wantNS, gotNS)
		})
	}
}

// decodeEnvelope decodes a rendered envelope for the given format (json or
// yaml — sigs.k8s.io/yaml marshals through JSON, so both round-trip through
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
			_, ce := runtime.Run(context.Background(), cmd, input{}, runtime.IO{Out: &buf, Format: format}, safety.Binding{})
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
			_, ce := runtime.Run(context.Background(), cmd, input{args: []string{"prod"}}, runtime.IO{Out: &buf, Format: format}, safety.Binding{})
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

// TestPlural pins the usage-string pluralizer against every ending the kind
// table can produce, including the sibilant and hyphenated cases the naive
// name+"s" got wrong ("ingresss").
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
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, c.want, plural(c.name))
		})
	}
}

func TestIsVowel(t *testing.T) {
	t.Parallel()
	for _, b := range []byte("aeiou") {
		assert.True(t, isVowel(b), "want vowel: %c", b)
	}
	for _, b := range []byte("bcdfgyz") {
		assert.False(t, isVowel(b), "want non-vowel: %c", b)
	}
}
