package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
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
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
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
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
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

// TestKindOverridesReplaceExactlyTheirVerb pins the override mechanism: an
// overridden verb+kind resolves to the override's command, every other verb
// of that kind and every other kind stays generic, and no command name is
// registered twice. With the empty kindOverrideTable this task ships, this
// is a pure regression net for later tasks' overrides.
func TestKindOverridesReplaceExactlyTheirVerb(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	names := map[string]int{}
	var walk func(cmds []*ucli.Command, prefix string)
	walk = func(cmds []*ucli.Command, prefix string) {
		for _, c := range cmds {
			names[strings.TrimSpace(prefix+" "+c.Name)]++
			walk(c.Commands, prefix+" "+c.Name)
		}
	}
	walk(root.Commands, "")
	for n, count := range names {
		assert.Equal(t, 1, count, "command %q registered %d times", n, count)
	}
}

// markerCmd stands in for a real override: Describe().Name matches the
// generic constructor's name (the override contract kinds.Bindings() depends
// on — BINDING NOTE in the plan) while Usage carries a marker string so
// tests can tell it apart from the generic command. It implements
// ReadCommand (not just Command) because reg.Validate requires every
// registered command to satisfy ReadCommand or MutatingCommand.
type markerCmd struct{ k kinds.Kind }

func (m markerCmd) Describe() runtime.Descriptor {
	return runtime.Descriptor{Name: "get " + m.k.Name, CommandType: runtime.ReadOnly, Usage: "MARKER"}
}

func (m markerCmd) Execute(context.Context, runtime.Input, runtime.Prompter) (runtime.Result, error) {
	return runtime.Result{}, nil
}

// TestKindOverrideSubstitutesExactlyItsVerb installs a marker override for
// one control kind and proves: the overridden verb resolves to the marker
// command, the kind's OTHER verbs stay generic, and an untouched kind is
// byte-identical. NOT t.Parallel — it mutates the package-level
// kindOverrideTable.
func TestKindOverrideSubstitutesExactlyItsVerb(t *testing.T) {
	orig := kindOverrideTable
	kindOverrideTable = map[string]kindOverride{
		"gitrepo": {get: func(k kinds.Kind, d runDeps) runtime.Command { return markerCmd{k} }},
	}
	t.Cleanup(func() { kindOverrideTable = orig })

	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	getGroup := findCommand(root.Commands, "get")
	require.NotNil(t, getGroup)
	gitrepoGet := findCommand(getGroup.Commands, "gitrepo")
	require.NotNil(t, gitrepoGet, "get gitrepo must still be registered")
	assert.Equal(t, "MARKER", gitrepoGet.Usage, "get gitrepo must resolve to the override marker, not the generic constructor")

	gitrepoKind, ok := kinds.Lookup("gitrepo")
	require.True(t, ok)
	genericPatch := resources.NewPatch(gitrepoKind, nil, nil).Describe()
	genericDelete := resources.NewDelete(gitrepoKind, nil).Describe()

	patchGroup := findCommand(root.Commands, "patch")
	require.NotNil(t, patchGroup)
	gitrepoPatch := findCommand(patchGroup.Commands, "gitrepo")
	require.NotNil(t, gitrepoPatch)
	assert.Equal(t, genericPatch.Usage, gitrepoPatch.Usage, "patch gitrepo must stay generic when only get is overridden")

	deleteGroup := findCommand(root.Commands, "delete")
	require.NotNil(t, deleteGroup)
	gitrepoDelete := findCommand(deleteGroup.Commands, "gitrepo")
	require.NotNil(t, gitrepoDelete)
	assert.Equal(t, genericDelete.Usage, gitrepoDelete.Usage, "delete gitrepo must stay generic when only get is overridden")

	bundleKind, ok := kinds.Lookup("bundle")
	require.True(t, ok)
	genericBundleGet := resources.NewGet(bundleKind, nil).Describe()
	bundleGet := findCommand(getGroup.Commands, "bundle")
	require.NotNil(t, bundleGet)
	assert.Equal(t, genericBundleGet.Usage, bundleGet.Usage, "get bundle (an untouched kind) must be byte-identical to the generic")
}

// TestSchemaManifestExposesProjectScope pins PS-6: a generated get command
// for a ProjectScopable kind (pod: NSPlain+Downstream) advertises
// projectScope: true, while a fixed command (schema itself) does not.
func TestSchemaManifestExposesProjectScope(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(context.Background(), []string{"rancher", "--config", dir, "schema", "-o", "json"}))

	var env struct {
		Data struct {
			Commands []struct {
				Name         string `json:"name"`
				ProjectScope bool   `json:"projectScope"`
			} `json:"commands"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))

	byName := map[string]bool{}
	for _, c := range env.Data.Commands {
		byName[c.Name] = c.ProjectScope
	}

	scopable, ok := byName["get pod"]
	require.True(t, ok, "get pod must be a registered command")
	assert.True(t, scopable, "pod is NSPlain+Downstream, so get pod must be projectScope: true")

	fixed, ok := byName["schema"]
	require.True(t, ok, "schema must be a registered command")
	assert.False(t, fixed, "a fixed command must not advertise projectScope")
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

// TestAdaptEntrySetsArgsUsageFromDescriptorArgs pins PS-9's second adapter
// site: adaptEntry (every generated get/patch/delete leaf) sets ArgsUsage
// from the same Descriptor.Args the manifest publishes, exactly like Adapt.
func TestAdaptEntrySetsArgsUsageFromDescriptorArgs(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)

	getGroup := findCommand(root.Commands, "get")
	require.NotNil(t, getGroup)
	pod := findCommand(getGroup.Commands, "pod")
	require.NotNil(t, pod)
	assert.Equal(t, "[name]", pod.ArgsUsage, "get pod's name argument is optional")

	delGroup := findCommand(root.Commands, "delete")
	require.NotNil(t, delGroup)
	delPod := findCommand(delGroup.Commands, "pod")
	require.NotNil(t, delPod)
	assert.Equal(t, "<name>", delPod.ArgsUsage, "delete always addresses a resource by name")
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
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
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
	assert.Equal(t, config.MinRancherVersion, data["minRancherVersion"])
	assert.Equal(t, false, data["readOnly"], "no config/env/flag read-only source is active for this invocation")
}

// TestSchemaReportsConfigSourcedReadOnly is the external-review regression:
// the schema command sits on the Adapt path that never calls resolveDeps, so
// a config-file-sourced readOnly must still reach the manifest — not just
// env/flag sources.
func TestSchemaReportsConfigSourcedReadOnly(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("readOnly: true\n"), 0o600))

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(context.Background(), []string{"rancher", "--config", dir, "schema", "-o", "json"}))

	var env struct {
		Data struct {
			ReadOnly        bool     `json:"readOnly"`
			ReadOnlySources []string `json:"readOnlySources"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.True(t, env.Data.ReadOnly)
	assert.Contains(t, env.Data.ReadOnlySources, "config readOnly")
}

// DR-006 repro: an unknown top-level command or an unknown kind under a group
// must surface the standard VALIDATION envelope at exit 2, not urfave's own
// unhinted "No help topic" text at exit 3 (indistinguishable from the AUTH
// exit code).
func TestUnknownCommandIsValidationNotExitThree(t *testing.T) {
	origExiter := ucli.OsExiter
	var gotCode int
	ucli.OsExiter = func(code int) { gotCode = code }
	defer func() { ucli.OsExiter = origExiter }()

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut

	require.NoError(t, root.Run(context.Background(), []string{"rancher", "bogus-command-xyz"}))
	assert.Equal(t, 2, gotCode)
	var env errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Message, "bogus-command-xyz")

	gotCode, errOut = 0, bytes.Buffer{}
	root.ErrWriter = &errOut
	require.NoError(t, root.Run(context.Background(), []string{"rancher", "get", "bogus-kind-xyz"}))
	assert.Equal(t, 2, gotCode)
	var env2 errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env2))
	assert.Equal(t, "VALIDATION", env2.Data.Error.Code)
}

// exercise bug 2 repro: unknown command "version" is the friction item — the
// hint must point at --version, not the generic `rancher schema` hint.
func TestUnknownVersionCommandHintsAtFlag(t *testing.T) {
	origExiter := ucli.OsExiter
	ucli.OsExiter = func(int) {}
	defer func() { ucli.OsExiter = origExiter }()

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut

	require.NoError(t, root.Run(context.Background(), []string{"rancher", "version"}))
	var env errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Hint, "--version")

	// A subgroup miss on the same literal name has no --version flag to
	// point at (that flag is root-only) — it must get the generic hint.
	errOut.Reset()
	require.NoError(t, root.Run(context.Background(), []string{"rancher", "get", "version"}))
	var env2 errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env2))
	assert.Equal(t, "VALIDATION", env2.Data.Error.Code)
	assert.NotContains(t, env2.Data.Error.Hint, "--version")
}

// exercise bug 2 repro: an unknown flag on a real command must surface the
// standard VALIDATION envelope at exit 2, not urfave's own unhinted
// "Incorrect Usage" text.
func TestUnknownFlagIsValidationEnvelope(t *testing.T) {
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

	runErr := root.Run(context.Background(), []string{"rancher", "get", "pod", "--bogus-flag"})
	require.Error(t, runErr)

	var env errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Message, "bogus-flag")
}

// An explicitly overridden config dir must not put a plain-text notice ahead
// of the error envelope — stderr must parse as pure JSON, exactly as a
// scripted session relies on.
func TestExplicitConfigDirKeepsStderrPureJSON(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	t.Setenv("RANCHER_CLI_URL", "")
	t.Setenv("RANCHER_CLI_TOKEN", "")
	dir := t.TempDir()
	t.Setenv("RANCHER_CLI_CONFIG_DIR", dir)

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut
	// The default handler calls os.Exit on a non-zero ExitCoder, which would kill
	// the test process; keep the error as a return value instead.
	root.ExitErrHandler = func(context.Context, *ucli.Command, error) {}

	runErr := root.Run(context.Background(), []string{"rancher", "get", "cluster", "foo", "-o", "json"})
	require.Error(t, runErr)

	stderr := errOut.Bytes()
	require.NotEmpty(t, stderr)
	var env map[string]any
	require.NoError(t, json.Unmarshal(stderr, &env), "stderr must be pure JSON, got %q", stderr)
	data := env["data"].(map[string]any)
	errObj := data["error"].(map[string]any)
	assert.Equal(t, "CONTEXT_NOT_SET", errObj["code"])
}

// TestCorruptConfigDoesNotBreakSchemaOrCompletionCallback pins the SC-1 hard
// requirement: resolveReadOnly already degrades to not-read-only on a config
// load error (adapter.go:129-143) rather than failing the command, on BOTH a
// before-only path (schema, Adapt-wired, no resolveDeps) and the completion
// callback (Before runs, then completion returns before Action). This must
// hold with no code change to resolveReadOnly/resolveDeps — the test only
// pins existing behavior against regression.
func TestCorruptConfigDoesNotBreakSchemaOrCompletionCallback(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("not: [valid yaml\n"), 0o600))

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(context.Background(), []string{"rancher", "--config", dir, "schema", "-o", "json"}))
	var env struct {
		Data struct {
			ReadOnly bool `json:"readOnly"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &env))
	assert.False(t, env.Data.ReadOnly, "a corrupt config must degrade to not-read-only, not fail schema")

	out.Reset()
	require.NoError(t, root.Run(context.Background(), []string{
		"rancher", "--config", dir, "get", "--generate-shell-completion",
	}))
	assert.NotEmpty(t, out.String(), "the completion callback must still emit candidates on a corrupt config, not fail")
}

// TestGenerateShellCompletionListsTopLevelCommandNames pins that
// EnableShellCompletion activates default command-name completion for the
// existing tree with no assembleCommands/wireEntry changes. Membership only —
// urfave-owned ordering/format is not ours to golden-test.
func TestGenerateShellCompletionListsTopLevelCommandNames(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	// urfave's DefaultCompleteWithFlags reads the real os.Args (not the argv
	// passed to Run) when invoked at the root command — it always assumes the
	// shell appended the completion flag last on the actual process args. Mirror
	// that so the test drives the same code path a real TAB keystroke does.
	argv := []string{"rancher", "--config", dir, "--generate-shell-completion"}
	origArgs := os.Args
	os.Args = argv
	t.Cleanup(func() { os.Args = origArgs })

	require.NoError(t, root.Run(context.Background(), argv))
	assert.Contains(t, out.String(), "get")
	assert.Contains(t, out.String(), "auth")
}

// TestHiddenCompletionCommandEmitsNoScriptMarkers pins that the renamed
// built-in (__completion) remains invocable as machinery only: its
// subcommands are stripped, so `rancher __completion bash` cannot emit
// urfave's own divergent script. This pins the INTENT, not urfave's own
// help-topic mechanism: post-strip, "bash" is an unmatched subcommand name on
// __completion, which now routes through the repo's standard CommandNotFound
// seam — a structured JSON VALIDATION envelope on stderr at a non-zero exit,
// the same envelope-everywhere contract every other unmatched command name
// gets (see TestUnknownCommandIsValidationNotExitThree), not urfave's own
// unhinted "No help topic" text and not a custom help Action (the command is
// machinery, not surface, per STOP adjudication 2026-07-12).
func TestHiddenCompletionCommandEmitsNoScriptMarkers(t *testing.T) {
	origExiter := ucli.OsExiter
	var gotCode int
	ucli.OsExiter = func(code int) { gotCode = code }
	defer func() { ucli.OsExiter = origExiter }()

	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out, errOut bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &errOut

	require.NoError(t, root.Run(context.Background(), []string{"rancher", "--config", dir, "__completion", "bash"}))
	assert.Equal(t, 2, gotCode, "an unmatched subcommand name is VALIDATION, exit 2, not urfave's own exit 3")

	var env errEnvelope
	require.NoError(t, json.Unmarshal(errOut.Bytes(), &env))
	assert.Equal(t, "VALIDATION", env.Data.Error.Code)
	assert.Contains(t, env.Data.Error.Message, "bash")

	combined := out.String() + errOut.String()
	assert.NotContains(t, combined, "--generate-shell-completion",
		"the hidden built-in must never emit urfave's own completion script")
}

func TestLoginThenContextCurrentEndToEnd(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
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
