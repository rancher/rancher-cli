package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

type pingCmd struct{}

func (pingCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "ping", CommandType: runtime.Local}
}
func (pingCmd) Plan(context.Context, runtime.Input) (safety.Effect, error) {
	return safety.Effect{}, nil
}
func (pingCmd) Execute(context.Context, runtime.Input, safety.Effect) (runtime.Result, error) {
	return runtime.Result{Kind: "Pong", Data: map[string]string{"msg": "pong"}}, nil
}

func TestAdaptRunsAndRendersJSON(t *testing.T) {
	var out bytes.Buffer
	root := &ucli.Command{
		Name:     "rancher",
		Writer:   &out,
		Flags:    GlobalFlags(),
		Commands: []*ucli.Command{Adapt(pingCmd{})},
	}
	err := root.Run(context.Background(), []string{"rancher", "ping", "-o", "json"})
	require.NoError(t, err)

	var env map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.Equal(t, "Pong", env["kind"])
}
