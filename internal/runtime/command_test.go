package runtime

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeCmd struct{}

func (fakeCmd) Describe() Descriptor {
	return Descriptor{Name: "widgets ls", Usage: "list", CommandType: ReadOnly}
}
func (fakeCmd) Plan(context.Context, Input) (safety.Effect, error) { return safety.Effect{}, nil }
func (fakeCmd) Execute(context.Context, Input, safety.Effect, *ui.Prompter) (Result, error) {
	return Result{Kind: "WidgetList", Data: []string{"a"}}, nil
}

func TestCommandInterfaceSatisfied(t *testing.T) {
	var c Command = fakeCmd{}
	assert.Equal(t, ReadOnly, c.Describe().CommandType)
	res, err := c.Execute(context.Background(), nil, safety.Effect{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "WidgetList", res.Kind)
}
