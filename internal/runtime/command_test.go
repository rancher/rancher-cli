package runtime

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadCommandContract(t *testing.T) {
	t.Parallel()
	var c Command = fakeRead{
		d:   Descriptor{Name: "widgets ls", Usage: "list", CommandType: ReadOnly},
		res: Result{Kind: "WidgetList", Data: []string{"a"}},
	}
	assert.Equal(t, ReadOnly, c.Describe().CommandType)
	rc, ok := c.(ReadCommand)
	require.True(t, ok, "fakeRead must satisfy ReadCommand")
	res, err := rc.Execute(t.Context(), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "WidgetList", res.Kind)
}

func TestMutatingCommandContract(t *testing.T) {
	t.Parallel()
	var c Command = fakeMutating{
		d:    Descriptor{Name: "clusters scale", CommandType: Mutating},
		eff:  safety.Effect{Action: safety.ActionUpdate, Target: "x"},
		exec: func(context.Context) (Result, error) { return Result{Kind: "ScaleResult"}, nil },
	}
	assert.Equal(t, Mutating, c.Describe().CommandType)
	mc, ok := c.(MutatingCommand)
	require.True(t, ok, "fakeMutating must satisfy MutatingCommand")
	eff, exec, err := mc.Plan(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, safety.ActionUpdate, eff.Action)
	require.NotNil(t, exec)
	res, err := exec(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "ScaleResult", res.Kind)
}
