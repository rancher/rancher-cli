package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ucli "github.com/urfave/cli/v3"
)

func TestBuildRootValidatesRegistry(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("test")
	if err != nil {
		t.Fatalf("BuildRoot on the real wiring must pass validation: %v", err)
	}
	if root == nil {
		t.Fatal("nil root")
	}
}

// fakeEngineCmd is a read command with a caller-chosen name, for exercising the
// registration loop's validation in isolation.
type fakeEngineCmd struct{ name string }

func (f fakeEngineCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: f.name, CommandType: runtime.ReadOnly}
}
func (f fakeEngineCmd) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return runtime.Result{}, nil
}

func findCommand(cmds []*ucli.Command, name string) *ucli.Command {
	for _, c := range cmds {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func schemaCommandNames(t *testing.T) []string {
	t.Helper()
	t.Setenv("RANCHER_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(context.Background(), []string{"rancher", "--config", dir, "schema", "-o", "json"}))

	var env struct {
		Data struct {
			Commands []struct {
				Name string `json:"name"`
			} `json:"commands"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	names := make([]string, 0, len(env.Data.Commands))
	for _, c := range env.Data.Commands {
		names = append(names, c.Name)
	}
	return names
}

var update = flag.Bool("update", false, "rewrite golden files")

func TestSchemaManifestGolden(t *testing.T) {
	t.Setenv("RANCHER_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("golden")
	require.NoError(t, err)

	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(context.Background(), []string{"rancher", "--config", dir, "schema", "-o", "json"}))

	// Re-marshal the manifest (the envelope's data) through a map so key order is
	// stable regardless of struct field order.
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	var manifest any
	require.NoError(t, json.Unmarshal(env.Data, &manifest))
	got, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')

	golden := filepath.Join("testdata", "schema.golden.json")
	if *update {
		require.NoError(t, os.WriteFile(golden, got, 0o644))
		return
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got), "schema manifest drifted from golden; run with -update if intended")
}

func TestBuildRootWiresGeneratedCommands(t *testing.T) {
	names := schemaCommandNames(t)
	assert.Contains(t, names, "get pod")
	assert.Contains(t, names, "delete project")
	assert.NotContains(t, names, "delete cluster", "clusters are not deletable")
}

func TestBuildRootWiresAliases(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	getGroup := findCommand(root.Commands, "get")
	require.NotNil(t, getGroup)
	pod := findCommand(getGroup.Commands, "pod")
	require.NotNil(t, pod, "get group must have a pod leaf")
	assert.Contains(t, pod.Aliases, "pods")
	assert.Contains(t, pod.Aliases, "po")

	delGroup := findCommand(root.Commands, "delete")
	require.NotNil(t, delGroup)
	project := findCommand(delGroup.Commands, "project")
	require.NotNil(t, project, "delete group must have a project leaf")
	assert.Contains(t, project.Aliases, "projects")
}

func TestAssembleCommandsUnknownGroupReachable(t *testing.T) {
	t.Parallel()
	leaf := &ucli.Command{Name: "widget"}
	out := assembleCommands(nil, []string{"update"}, map[string][]*ucli.Command{"update": {leaf}})
	grp := findCommand(out, "update")
	require.NotNil(t, grp, "a group not in the usage allowlist must still be reachable")
	require.NotNil(t, findCommand(grp.Commands, "widget"))
}

func TestWireEntryRejectsThreeTokenName(t *testing.T) {
	t.Parallel()
	resolver, err := runtime.ComposeResolvers(provider.NewResolver(), runtime.MapResolver(kinds.Bindings()))
	require.NoError(t, err)
	reg := runtime.NewRegistry(resolver)

	bad := entry{group: "get", make: func(runDeps) runtime.Command {
		return fakeEngineCmd{name: "get pod extra"}
	}}
	_, werr := wireEntry(reg, resolver, bad)
	require.Error(t, werr)
	assert.Contains(t, werr.Error(), "token")
}

func TestBuildRootFailsOnEngineEntryWithoutBinding(t *testing.T) {
	t.Parallel()
	resolver, err := runtime.ComposeResolvers(provider.NewResolver(), runtime.MapResolver(kinds.Bindings()))
	require.NoError(t, err)
	reg := runtime.NewRegistry(resolver)

	// An engine entry whose Describe().Name is absent from the composed resolver
	// must fail wiring (deferral (a): read OR mutating engine commands need a
	// binding). The name's group matches so the leaf/group check passes first.
	bad := entry{group: "get", make: func(runDeps) runtime.Command {
		return fakeEngineCmd{name: "get gizmo"}
	}}
	_, werr := wireEntry(reg, resolver, bad)
	require.Error(t, werr)
	assert.Contains(t, werr.Error(), "binding")
}

func TestSchemaCommandEndToEnd(t *testing.T) {
	t.Setenv("RANCHER_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	var out bytes.Buffer
	root.Writer = &out
	err = root.Run(context.Background(), []string{"rancher", "--config", dir, "schema", "-o", "json"})
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &m))
	data := m["data"].(map[string]any)
	assert.Equal(t, "cli.rancher.io/v1", data["outputApiVersion"])
	assert.NotEmpty(t, data["globalFlags"])
}

func TestLoginThenContextCurrentEndToEnd(t *testing.T) {
	t.Setenv("RANCHER_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	root.Reader = bytes.NewBufferString("id:secret\n")

	require.NoError(t, root.Run(context.Background(), []string{
		"rancher", "--config", dir, "auth", "login", "--server", "https://r.ex",
		"--token-stdin", "--name", "acme", "--context", "c-1:p-1", "-o", "json",
	}))

	_, statErr := os.Stat(filepath.Join(dir, "config.yaml"))
	require.NoError(t, statErr, "login must write config.yaml under --config dir")

	_, statErr2 := os.Stat(filepath.Join(dir, "cli2.json"))
	require.True(t, os.IsNotExist(statErr2), "must not write cli2.json")

	cfg, cfgErr := config.Load(config.FilePath(dir))
	require.NoError(t, cfgErr)
	assert.Equal(t, "c-1", cfg.Contexts["acme"].Cluster,
		"auth login's local --context flag must still land in the written config (CW-3: leaf shadows the global --context flag)")
	assert.Equal(t, "p-1", cfg.Contexts["acme"].Project)

	out.Reset()
	require.NoError(t, root.Run(context.Background(), []string{
		"rancher", "--config", dir, "config", "current-context", "-o", "json",
	}))
	var env map[string]any
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.Equal(t, "Context", env["kind"])
}
