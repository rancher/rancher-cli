package runtime

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tin is a minimal Input stub for tests that only need positional Args.
type tin struct{ a []string }

func (t tin) String(string) string { return "" }
func (t tin) Bool(string) bool     { return false }
func (t tin) Args() []string       { return t.a }

func TestSchemaFilter(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get pod", Aliases: []string{"pods"}, CommandType: ReadOnly}})
	reg.Register(fakeRead{d: Descriptor{Name: "auth login", CommandType: Local}})
	cmd := NewSchemaCommand(reg, "test", "2.14", nil)

	t.Run("no args returns the full manifest", func(t *testing.T) {
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{}, nil)
		require.NoError(t, err)
		m := res.Data.(Manifest)
		assert.Len(t, m.Commands, 2)
	})

	t.Run("multi-word name filters to one", func(t *testing.T) {
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"get", "pod"}}, nil)
		require.NoError(t, err)
		m := res.Data.(Manifest)
		require.Len(t, m.Commands, 1)
		assert.Equal(t, "get pod", m.Commands[0].Name)
		assert.NotEmpty(t, m.ErrorCodes, "filtered manifest keeps the shared context")
	})

	t.Run("alias matches", func(t *testing.T) {
		res, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"get", "pods"}}, nil)
		require.NoError(t, err)
		assert.Equal(t, "get pod", res.Data.(Manifest).Commands[0].Name)
	})

	t.Run("unknown command is VALIDATION", func(t *testing.T) {
		_, err := cmd.(ReadCommand).Execute(context.Background(), tin{a: []string{"get", "nonsense"}}, nil)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	})
}

func TestSchemaReportsReadOnlyFromResolver(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get pod", CommandType: ReadOnly}})
	cmd := NewSchemaCommand(reg, "test", "2.14", func() (bool, []string) {
		return true, []string{"config readOnly"}
	})

	res, err := cmd.(ReadCommand).Execute(context.Background(), tin{}, nil)
	require.NoError(t, err)
	m := res.Data.(Manifest)
	assert.True(t, m.ReadOnly)
	assert.Equal(t, []string{"config readOnly"}, m.ReadOnlySources)
	assert.Equal(t, "2.14", m.MinRancherVersion)
}

func TestSchemaReadOnlyFalseWhenResolverNil(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(stubResolver{})
	reg.Register(fakeRead{d: Descriptor{Name: "get pod", CommandType: ReadOnly}})
	cmd := NewSchemaCommand(reg, "test", "2.14", nil)

	res, err := cmd.(ReadCommand).Execute(context.Background(), tin{}, nil)
	require.NoError(t, err)
	m := res.Data.(Manifest)
	assert.False(t, m.ReadOnly)
	assert.Empty(t, m.ReadOnlySources)
}
