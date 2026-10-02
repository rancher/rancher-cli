package access

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// fakeInput is a minimal runtime.Input for command tests.
type fakeInput struct {
	strs  map[string]string
	bools map[string]bool
	args  []string
}

func (f fakeInput) String(n string) string    { return f.strs[n] }
func (f fakeInput) Bool(n string) bool        { return f.bools[n] }
func (f fakeInput) Strings(n string) []string { return nil }
func (f fakeInput) IsSet(n string) bool       { _, ok := f.strs[n]; return ok || f.bools[n] }
func (f fakeInput) Args() []string            { return f.args }

var _ runtime.Input = fakeInput{}

func nsObj(name string, labels map[string]string) unstructured.Unstructured {
	l := map[string]any{}
	for k, v := range labels {
		l[k] = v
	}
	return unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace",
		"metadata": map[string]any{"name": name, "labels": l}}}
}

func scopeClient(namespaces ...string) *fakeClient {
	items := make([]unstructured.Unstructured, 0, len(namespaces))
	objs := map[string]*unstructured.Unstructured{}
	for _, n := range namespaces {
		o := nsObj(n, map[string]string{"field.cattle.io/projectId": "p-cvrp4"})
		items = append(items, o)
		oc := o
		objs[n] = &oc
	}
	return &fakeClient{Objects: objs, Lists: map[string]*unstructured.UnstructuredList{
		"namespaces||": {Items: items},
		"namespaces||field.cattle.io/projectId=p-cvrp4": {Items: items},
	}}
}

// scopeJSON is the wire form of a Scope, for key-presence assertions.
func scopeJSON(t *testing.T, sc Scope) map[string]any {
	t.Helper()
	raw, err := json.Marshal(sc)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

func TestValidateSelectorsIsPure(t *testing.T) {
	t.Parallel()
	require.NoError(t, validateSelectors(scopeFlags{}))
	require.NoError(t, validateSelectors(scopeFlags{namespace: "team-a"}))
	err := validateSelectors(scopeFlags{namespace: "team-a", project: "p-cvrp4"})
	require.ErrorContains(t, err, "at most one of")
	err = validateSelectors(scopeFlags{allNamespaces: true})
	require.ErrorContains(t, err, "--all-namespaces is not accepted here")
	require.NoError(t, validateSelectors(scopeFlags{allNamespaces: true, allowAll: true}))
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestRejectSelectorsOnClusterScopedResource(t *testing.T) {
	t.Parallel()
	nodes := ResourceRef{Group: "", Version: "v1", Resource: "nodes", Namespaced: false}
	for _, fl := range []scopeFlags{{namespace: "team-a"}, {project: "p-cvrp4"}, {allNamespaces: true, allowAll: true}} {
		require.ErrorContains(t, nodes.rejectSelectors(fl), "resource nodes is cluster-scoped")
	}
	require.NoError(t, nodes.rejectSelectors(scopeFlags{}))
	pods := ResourceRef{Group: "", Version: "v1", Resource: "pods", Namespaced: true}
	require.NoError(t, pods.rejectSelectors(scopeFlags{project: "p-cvrp4"}))
}

func TestResolveScopeNamespaceMustExist(t *testing.T) {
	t.Parallel()
	c := scopeClient("team-a")
	sc, err := resolveScope(t.Context(), c, "c-phmj7", scopeFlags{namespace: "team-a"}, runtime.Project{})
	require.NoError(t, err)
	assert.Equal(t, "c-phmj7", sc.Cluster)
	assert.Equal(t, []string{"team-a"}, sc.targets)
	assert.Equal(t, scopeNamespace, sc.mode)
	m := scopeJSON(t, sc)
	assert.Equal(t, "team-a", m["namespace"])
	_, hasList := m["namespaces"]
	assert.False(t, hasList, "targets are not wire scope")
	_, err = resolveScope(t.Context(), c, "c-phmj7", scopeFlags{namespace: "ghost"}, runtime.Project{})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "namespace ghost not found on cluster c-phmj7")
}

// TestResolveScopeProjectAndAllNamespacesTakeListedNames asserts the
// targets are the names the project's namespace read and the -A LIST
// return, and that the scope records the project's id even when the
// typed --project was its display name.
func TestResolveScopeProjectAndAllNamespacesTakeListedNames(t *testing.T) {
	t.Parallel()
	c := scopeClient("team-b", "team-a")
	project := clientProject(c, "p-cvrp4", "Team")
	sc, err := resolveScope(t.Context(), c, "c-phmj7", scopeFlags{project: "Team"}, project)
	require.NoError(t, err)
	assert.Equal(t, []string{"team-a", "team-b"}, sc.targets, "sorted, as listed")
	assert.Equal(t, []string{"team-a", "team-b"}, sc.Namespaces, "project members are wire scope")
	assert.Equal(t, "p-cvrp4", sc.Project)
	m := scopeJSON(t, sc)
	assert.Equal(t, "p-cvrp4", m["project"])
	_, hasNS := m["namespace"]
	assert.False(t, hasNS)

	sc, err = resolveScope(t.Context(), c, "c-phmj7", scopeFlags{allNamespaces: true, allowAll: true}, runtime.Project{})
	require.NoError(t, err)
	assert.Equal(t, []string{"team-a", "team-b"}, sc.targets)
	assert.Nil(t, sc.Namespaces, "-A lists nothing in scope; rows carry the names")
	assert.Equal(t, true, scopeJSON(t, sc)["allNamespaces"])

	// No per-namespace GET happened: the fake would have failed on a GET
	// for a name absent from Objects; make one absent and assert success.
	c.Objects = map[string]*unstructured.Unstructured{}
	sc, err = resolveScope(t.Context(), c, "c-phmj7", scopeFlags{project: "p-cvrp4"}, project)
	require.NoError(t, err)
	assert.Len(t, sc.targets, 2)

	c.Lists["namespaces||field.cattle.io/projectId=p-empty"] = &unstructured.UnstructuredList{}
	sc, err = resolveScope(t.Context(), c, "c-phmj7", scopeFlags{project: "p-empty"}, clientProject(c, "p-empty", "Empty"))
	require.NoError(t, err)
	assert.Empty(t, sc.targets, "an empty project is a legal empty target set")
	assert.Equal(t, scopeProject, sc.mode)
}

func TestResolveScopeClusterMode(t *testing.T) {
	t.Parallel()
	sc, err := resolveScope(t.Context(), scopeClient(), "c-phmj7", scopeFlags{}, runtime.Project{})
	require.NoError(t, err)
	assert.Equal(t, scopeCluster, sc.mode)
	assert.Nil(t, sc.targets)
	assert.Equal(t, map[string]any{"cluster": "c-phmj7"}, scopeJSON(t, sc))
}

// TestResolveScopeFollowsTheProjectRead asserts the project scope comes from
// the project the command read, not from the typed string: the typed value
// only feeds the selector checks, and with no project read the scope is
// the cluster's.
func TestResolveScopeFollowsTheProjectRead(t *testing.T) {
	t.Parallel()
	c := scopeClient("team-b", "team-a")
	sc, err := resolveScope(t.Context(), c, "c-phmj7", scopeFlags{}, clientProject(c, "p-cvrp4", "Team"))
	require.NoError(t, err)
	assert.Equal(t, "p-cvrp4", sc.Project)
	assert.Equal(t, []string{"team-a", "team-b"}, sc.targets)

	sc, err = resolveScope(t.Context(), c, "c-phmj7", scopeFlags{project: "Team"}, runtime.Project{})
	require.NoError(t, err)
	assert.Empty(t, sc.Project)
	assert.Equal(t, scopeCluster, sc.mode)
}
