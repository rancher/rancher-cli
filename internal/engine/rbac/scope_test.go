package rbac

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		_, err := ResolveScope(t.Context(), fakeFactory{c: &fakeClient{}}, stubInput{strings: strs, bools: map[string]bool{"global": true}})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	}
}

func TestResolveScopeNoneIsValidationError(t *testing.T) {
	_, err := ResolveScope(t.Context(), fakeFactory{c: &fakeClient{}}, stubInput{})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestResolveScopeCluster(t *testing.T) {
	// ResolveDownstreamCluster resolves the explicit flag; with a bare id
	// and no clusters listed it may pass the id through or resolve it —
	// read internal/engine/route.go before finalizing this fake's data.
	got, err := ResolveScope(t.Context(), fakeFactory{c: &fakeClient{}}, stubInput{strings: map[string]string{"cluster": "c-x"}})
	require.NoError(t, err)
	assert.Equal(t, ScopeCluster, got.Kind)
	assert.Equal(t, "c-x", got.Cluster)
	assert.Equal(t, "c-x", got.NS)
	assert.Equal(t, "clusterroletemplatebinding", got.KindKey)
	assert.Equal(t, "roleTemplateName", got.RoleField)
}
