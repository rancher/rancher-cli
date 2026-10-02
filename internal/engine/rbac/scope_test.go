package rbac

import (
	"context"
	"errors"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type stubInput struct {
	strings map[string]string
	bools   map[string]bool
	args    []string
}

func (s stubInput) String(name string) string { return s.strings[name] }
func (s stubInput) Bool(name string) bool     { return s.bools[name] }
func (s stubInput) Strings(string) []string   { return nil }
func (s stubInput) IsSet(string) bool         { return false }
func (s stubInput) Args() []string            { return s.args }

// typedProjectContext is the context the pipeline gives a command whose
// --project is typed: the project resolved by id on the typed --cluster.
// With no --project typed it is the test's context alone.
func typedProjectContext(t *testing.T, in runtime.Input) context.Context {
	t.Helper()
	id := in.String(runtime.FlagProject)
	if id == "" {
		return t.Context()
	}
	return runtime.WithProject(t.Context(), runtime.Project{Cluster: in.String(engine.FlagCluster), ID: id})
}

func TestResolveScopeGlobal(t *testing.T) {
	got, err := ResolveScope(t.Context(), fakeFactory{c: &fakeClient{}}, stubInput{bools: map[string]bool{"global": true}})
	require.NoError(t, err)
	assert.Equal(t, Scope{Kind: ScopeGlobal, KindKey: "globalrolebinding", RoleField: "globalRoleName"}, got)
}

func TestResolveScopeConflicts(t *testing.T) {
	tests := []map[string]string{
		{"project": "Default"}, // with global below
		{"cluster": "c-x"},     // with global below
	}
	for _, strs := range tests {
		ctx := runtime.WithProjectLookup(t.Context(), func(context.Context) (runtime.Project, error) {
			t.Error("the project was read before the scope conflict was refused")
			return runtime.Project{}, errors.New("unexpected project read")
		})
		_, err := ResolveScope(ctx, fakeFactory{cErr: errors.New("unexpected cluster client")}, stubInput{strings: strs, bools: map[string]bool{"global": true}})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	}
}

// TestResolveScopeProjectFromContext asserts the project scope takes the
// project id the pipeline resolved, not the typed value, and reads the
// backing namespace of that id.
func TestResolveScopeProjectFromContext(t *testing.T) {
	t.Parallel()
	client := &fakeClient{gets: map[string]*unstructured.Unstructured{
		"c-x/p-1": {Object: map[string]any{"metadata": map[string]any{"name": "p-1"}, "status": map[string]any{"backingNamespace": "c-x-p-1"}}},
	}}
	ctx := runtime.WithProject(t.Context(), runtime.Project{Cluster: "c-x", ID: "p-1", DisplayName: "web"})
	got, err := ResolveScope(ctx, fakeFactory{c: client}, stubInput{strings: map[string]string{"project": "web", "cluster": "c-x"}})
	require.NoError(t, err)
	assert.Equal(t, Scope{Kind: ScopeProject, Cluster: "c-x", ProjectID: "p-1", NS: "c-x-p-1",
		KindKey: "projectroletemplatebinding", RoleField: "roleTemplateName"}, got)
	assert.Zero(t, client.lists, "the scope lists nothing to find the project")
}

// TestResolveScopeTypedProjectWithoutContextIsCommandBug asserts a typed
// --project with no project on the context is a GENERIC command bug, never
// a scope built from the typed value.
func TestResolveScopeTypedProjectWithoutContextIsCommandBug(t *testing.T) {
	t.Parallel()
	client := &fakeClient{gets: map[string]*unstructured.Unstructured{
		"c-x/p-x": {Object: map[string]any{"metadata": map[string]any{"name": "p-x"}}},
	}}
	_, err := ResolveScope(t.Context(), fakeFactory{c: client}, stubInput{strings: map[string]string{"project": "p-x", "cluster": "c-x"}})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	require.ErrorContains(t, err, "command bug")
}

func TestResolveScopeNoneIsValidationError(t *testing.T) {
	_, err := ResolveScope(t.Context(), fakeFactory{c: &fakeClient{}}, stubInput{})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestResolveScopeCluster(t *testing.T) {
	// ResolveDownstreamCluster resolves the explicit flag; with a bare id
	// and no clusters listed it may pass the id through or resolve it -
	// read internal/engine/route.go before finalizing this fake's data.
	got, err := ResolveScope(t.Context(), fakeFactory{c: &fakeClient{}}, stubInput{strings: map[string]string{"cluster": "c-x"}})
	require.NoError(t, err)
	assert.Equal(t, ScopeCluster, got.Kind)
	assert.Equal(t, "c-x", got.Cluster)
	assert.Equal(t, "c-x", got.NS)
	assert.Equal(t, "clusterroletemplatebinding", got.KindKey)
	assert.Equal(t, "roleTemplateName", got.RoleField)
}

// TestResolveScopeNoScopeNamesTheFormsThatWork asserts the refusal of an
// unscoped grant or revoke names only flag sets that resolve: --project
// needs --cluster, so it is named with it.
func TestResolveScopeNoScopeNamesTheFormsThatWork(t *testing.T) {
	t.Parallel()
	_, err := ResolveScope(t.Context(), nil, stubInput{})
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "a scope is required: --project with --cluster, --cluster, or --global", ce.Message)
}
