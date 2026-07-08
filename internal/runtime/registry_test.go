package runtime

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubResolver map[string]safety.Binding

func (s stubResolver) Resolve(op string) (safety.Binding, bool) { b, ok := s[op]; return b, ok }
func (s stubResolver) All() map[string]safety.Binding           { return s }

func TestSchemaMergesResolverTier(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{
		"get cluster": {Tier: safety.TierCRD, Stability: safety.Beta},
	})
	reg.Register(fakeRead{d: Descriptor{Name: "widgets ls", Usage: "list", CommandType: ReadOnly}})
	reg.Register(fakeRead{d: Descriptor{Name: "get cluster", CommandType: ReadOnly}})
	reg.Register(fakeRead{d: Descriptor{Name: "widgets set", CommandType: Local,
		Flags: []ArgSpec{{Name: "token", Type: "string", Required: true}}}})

	m := reg.Schema("v-test")
	assert.Equal(t, "cli.rancher.io/v1", m.OutputAPIVersion)
	assert.NotEmpty(t, m.ErrorCodes)
	assert.NotEmpty(t, m.GlobalFlags)

	byName := map[string]SchemaCommand{}
	for _, c := range m.Commands {
		byName[c.Name] = c
	}
	require.Contains(t, byName, "get cluster")
	assert.Equal(t, "crd", byName["get cluster"].Tier) // merged from resolver

	// Per-command Flags must reach the manifest (agent discoverability).
	require.Contains(t, byName, "widgets set")
	require.Len(t, byName["widgets set"].Flags, 1)
	assert.Equal(t, "token", byName["widgets set"].Flags[0].Name)
	assert.True(t, byName["widgets set"].Flags[0].Required)
}

func TestValidateRejectsDuplicateNames(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get cluster", CommandType: ReadOnly}})
	reg.Register(fakeRead{d: Descriptor{Name: "get cluster", CommandType: ReadOnly}})
	err := reg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate")
}

func TestValidateRejectsMutatingWithoutBinding(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{}) // no binding for "create widget"
	reg.Register(fakeMutating{d: Descriptor{Name: "create widget", CommandType: Mutating}})
	err := reg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no resolver binding")
}

func TestValidateRejectsCommandTypeMismatch(t *testing.T) {
	t.Parallel()
	t.Run("read command declared mutating", func(t *testing.T) {
		reg := NewRegistry(stubResolver{"x": {Tier: safety.TierPublic, Stability: safety.Stable}})
		reg.Register(fakeRead{d: Descriptor{Name: "x", CommandType: Mutating}})
		err := reg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "commandType")
	})
	t.Run("mutating command declared readOnly", func(t *testing.T) {
		reg := NewRegistry(stubResolver{})
		reg.Register(fakeMutating{d: Descriptor{Name: "y", CommandType: ReadOnly}})
		err := reg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "commandType")
	})
}

type fakeBoth struct{ d Descriptor }

func (f fakeBoth) Describe() Descriptor { return f.d }
func (f fakeBoth) Execute(context.Context, Input, Prompter) (Result, error) {
	return Result{}, nil
}
func (f fakeBoth) Plan(context.Context, Input) (safety.Effect, ExecuteFunc, error) {
	return safety.Effect{}, nil, nil
}

func TestValidateRejectsDualInterface(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{"do thing": {}})
	reg.Register(fakeBoth{d: Descriptor{Name: "do thing", CommandType: Mutating}})
	if err := reg.Validate(); err == nil {
		t.Fatal("dual ReadCommand+MutatingCommand implementer must fail validation")
	}
}

func TestValidatePassesWellFormedRegistry(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{
		"create widget": {Tier: safety.TierPublic, Stability: safety.Stable},
	})
	reg.Register(fakeRead{d: Descriptor{Name: "get cluster", CommandType: ReadOnly}})
	reg.Register(fakeMutating{d: Descriptor{Name: "create widget", CommandType: Mutating}})
	require.NoError(t, reg.Validate())
}
