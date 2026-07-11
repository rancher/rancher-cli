package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

type pingCmd struct{}

func (pingCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "ping", CommandType: runtime.Local}
}
func (pingCmd) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return runtime.Result{Kind: "Pong", Data: map[string]string{"msg": "pong"}}, nil
}

func TestAdaptRunsAndRendersJSON(t *testing.T) {
	var out bytes.Buffer
	reg := runtime.NewRegistry(provider.NewResolver())
	root := &ucli.Command{
		Name:     "rancher",
		Writer:   &out,
		Flags:    GlobalFlags(),
		Commands: []*ucli.Command{Adapt(reg, pingCmd{})},
	}
	err := root.Run(context.Background(), []string{"rancher", "ping", "-o", "json"})
	require.NoError(t, err)

	var env map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.Equal(t, "Pong", env["kind"])
}

// TestArgsUsageRendersRequiredAndOptionalArgs pins PS-9's helper: required
// Args render "<name>", optional Args render "[name]", space-joined in
// declared order — the SAME Descriptor.Args the manifest publishes, so help
// text cannot drift from schema.
func TestArgsUsageRendersRequiredAndOptionalArgs(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "", argsUsage(nil))
	assert.Equal(t, "[command]", argsUsage([]runtime.ArgSpec{{Name: "command", Type: "string"}}))
	assert.Equal(t, "<name>", argsUsage([]runtime.ArgSpec{{Name: "name", Type: "string", Required: true}}))
	assert.Equal(t, "<name> [extra]", argsUsage([]runtime.ArgSpec{
		{Name: "name", Type: "string", Required: true},
		{Name: "extra", Type: "string"},
	}))
}

// TestAdaptSetsArgsUsageFromDescriptorArgs pins the Adapt wiring: the urfave
// ArgsUsage field is derived from the same Descriptor.Args the manifest
// already publishes.
func TestAdaptSetsArgsUsageFromDescriptorArgs(t *testing.T) {
	t.Parallel()
	reg := runtime.NewRegistry(provider.NewResolver())
	cmd := Adapt(reg, schemaLikeCmd{})
	assert.Equal(t, "[command]", cmd.ArgsUsage)
}

// schemaLikeCmd stands in for the real `schema` command's Descriptor (an
// optional positional arg) without depending on internal/runtime's schema_cmd
// wiring.
type schemaLikeCmd struct{}

func (schemaLikeCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{
		Name: "schema", CommandType: runtime.ReadOnly,
		Args: []runtime.ArgSpec{{Name: "command", Type: "string"}},
	}
}
func (schemaLikeCmd) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return runtime.Result{}, nil
}

// resolveReadOnlyFlags is the minimal flag set resolveReadOnly reads from.
func resolveReadOnlyFlags() []ucli.Flag {
	return []ucli.Flag{&ucli.StringFlag{Name: "config"}, &ucli.BoolFlag{Name: "read-only"}}
}

func runResolveReadOnly(t *testing.T, argv []string) (bool, []string) {
	t.Helper()
	var gotActive bool
	var gotSources []string
	cmd := &ucli.Command{
		Name:  "x",
		Flags: resolveReadOnlyFlags(),
		Action: func(_ context.Context, c *ucli.Command) error {
			gotActive, gotSources = resolveReadOnly(c)
			return nil
		},
	}
	require.NoError(t, cmd.Run(context.Background(), argv))
	return gotActive, gotSources
}

func TestResolveReadOnlyNoSourcesActive(t *testing.T) {
	active, sources := runResolveReadOnly(t, []string{"x", "--config", t.TempDir()})
	assert.False(t, active)
	assert.Empty(t, sources)
}

func TestResolveReadOnlyFlagSource(t *testing.T) {
	active, sources := runResolveReadOnly(t, []string{"x", "--config", t.TempDir(), "--read-only"})
	assert.True(t, active)
	assert.Equal(t, []string{"--read-only"}, sources)
}

func TestResolveReadOnlyEnvSourceNonEmptyIncludingFalse(t *testing.T) {
	t.Setenv("RANCHER_CLI_READONLY", "false")
	active, sources := runResolveReadOnly(t, []string{"x", "--config", t.TempDir()})
	assert.True(t, active, "RANCHER_CLI_READONLY=false is still non-empty, so the one-way rule turns read-only ON")
	assert.Equal(t, []string{"RANCHER_CLI_READONLY"}, sources)
}

func TestResolveReadOnlyConfigSource(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("readOnly: true\n"), 0o600))
	active, sources := runResolveReadOnly(t, []string{"x", "--config", dir})
	assert.True(t, active)
	assert.Equal(t, []string{"config readOnly"}, sources)
}

func TestResolveReadOnlyAllThreeCombineOrdered(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("readOnly: true\n"), 0o600))
	t.Setenv("RANCHER_CLI_READONLY", "1")
	_, sources := runResolveReadOnly(t, []string{"x", "--config", dir, "--read-only"})
	assert.Equal(t, []string{"config readOnly", "RANCHER_CLI_READONLY", "--read-only"}, sources)
}

func TestBuildIOReadsReadOnlyOverride(t *testing.T) {
	t.Cleanup(func() { config.SetReadOnlyOverride(false, nil) })
	config.SetReadOnlyOverride(true, []string{"RANCHER_CLI_READONLY"})

	var got runtime.IO
	cmd := &ucli.Command{
		Name: "x",
		Action: func(_ context.Context, c *ucli.Command) error {
			got = buildIO(c)
			return nil
		},
	}
	require.NoError(t, cmd.Run(context.Background(), []string{"x"}))
	assert.True(t, got.ReadOnly)
	assert.Equal(t, []string{"RANCHER_CLI_READONLY"}, got.ReadOnlySources)
}
