package cli

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeCompletionTestConfig(t *testing.T, dir string) {
	t.Helper()
	cfg := &config.Config{
		Contexts: map[string]config.Context{
			"zebra-ctx": {Server: "zebra-srv"},
			"alpha-ctx": {Server: "alpha-srv"},
			"mid-ctx":   {Server: "alpha-srv"},
		},
		Servers: map[string]config.Server{
			"zebra-srv": {URL: "https://z.example"},
			"alpha-srv": {URL: "https://a.example"},
		},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
}

// TestUseContextCompletionCallbackDrivesRealAdaptEntryWiring is the wiring
// test the plan calls for: it drives the actual adaptEntry-installed
// ShellComplete callback through BuildRoot/root.Run, not a hand-built
// completer. adaptEntry type-asserts the zero-deps probe (dir == ""); if the
// wiring (or contextUseCmd.CompleteArgs) ever read receiver state instead of
// the dir parameter the callback resolves at TAB time, this would return
// empty despite a real, populated config dir.
func TestUseContextCompletionCallbackDrivesRealAdaptEntryWiring(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	writeCompletionTestConfig(t, dir)

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(context.Background(), []string{
		"rancher", "--config", dir, "config", "use-context", "--generate-shell-completion",
	}))
	// Output order is pinned (external review finding 2): contexts are Go map
	// keys, so an exact-order assertion is the only thing that would catch a
	// nondeterminism regression — a membership-only assertion would not.
	assert.Equal(t, "alpha-ctx\nmid-ctx\nzebra-ctx\n", out.String())
}

// TestUseContextCompletionCallbackHasNoSideEffects mirrors the completion
// command's own no-side-effects regression (Task 2): the TAB-time callback
// must never touch config/store/pin. It deliberately leaves the credential
// store policy at its "auto" default — forcing "file" like most tests in
// this package would mask a completer that accidentally called resolveDeps/
// OpenStore instead of the pure config.Load the ShellCompleter contract
// requires.
func TestUseContextCompletionCallbackHasNoSideEffects(t *testing.T) {
	t.Setenv(config.EnvCredentialStore, "")
	dir := t.TempDir()
	writeCompletionTestConfig(t, dir)
	before, err := os.ReadFile(config.FilePath(dir))
	require.NoError(t, err)

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(context.Background(), []string{
		"rancher", "--config", dir, "config", "use-context", "--generate-shell-completion",
	}))
	require.NotEmpty(t, out.String())

	after, err := os.ReadFile(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, before, after, "the TAB callback must never write config.yaml")

	_, pinErr := os.Stat(config.PinPath(dir))
	assert.True(t, os.IsNotExist(pinErr), "the TAB callback must never write credstore-pin (would indicate a resolveDeps/OpenStore path)")
}

// TestUseContextCompletionCallbackEmptyOnCorruptConfig pins the same
// missing/corrupt-config degrade-to-empty contract as the unit-level
// CompleteArgs tests, but through the real callback end to end.
func TestUseContextCompletionCallbackEmptyOnCorruptConfig(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("not: [valid yaml\n"), 0o600))

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out
	require.NoError(t, root.Run(context.Background(), []string{
		"rancher", "--config", dir, "config", "use-context", "--generate-shell-completion",
	}))
	assert.Empty(t, out.String())
}

// TestGetGroupCompletionListsAliases pins the alias-aware override: `get
// <TAB>` must list each kind leaf's Aliases alongside its canonical name —
// urfave's own default listing (printCommandSuggestions) prints only the
// canonical Name, so `po<TAB>` would otherwise complete solely toward "pod".
func TestGetGroupCompletionListsAliases(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(context.Background(), []string{"rancher", "--config", dir, "get", "--generate-shell-completion"}))
	assert.Contains(t, out.String(), "pod:")
	assert.Contains(t, out.String(), "pods:")
	assert.Contains(t, out.String(), "po:")
}

// TestGetGroupCompletionFlagBranchPreserved pins that the alias override
// only replaces the subcommand-listing branch: a "-"-prefixed current word
// still defers to urfave's own flag-suggestion behavior, not the alias
// listing. The `get` group command owns no flags itself (they live on each
// kind leaf), so upstream's own default already produces empty output here
// — the same empty result before and after this override is what "preserved"
// means; the regression this guards against is the override always
// printing aliases regardless of branch.
func TestGetGroupCompletionFlagBranchPreserved(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(context.Background(), []string{"rancher", "--config", dir, "get", "--dr", "--generate-shell-completion"}))
	assert.Empty(t, out.String(), "a flag-prefixed current word must not fall through to alias listing")
}

// TestInspectGroupCompletionHasAliasOverride pins that inspect/pod,
// deployment, and project leaves now carry Aliases, so the alias-aware
// completion override must be installed on the inspect group to properly
// suggest and dispatch those aliases.
func TestInspectGroupCompletionHasAliasOverride(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	inspectGroup := findCommand(root.Commands, "inspect")
	require.NotNil(t, inspectGroup)
	assert.NotNil(t, inspectGroup.ShellComplete, "inspect leaves now carry aliases; the override must be installed for alias dispatch")
}

// TestInspectGroupCompletionListsAliases pins the alias-aware override on the
// inspect group: `inspect <TAB>` must list each leaf's Aliases alongside its
// canonical name, so "po" and "deploy" appear for the pod and deployment leaves.
func TestInspectGroupCompletionListsAliases(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(context.Background(), []string{"rancher", "--config", dir, "inspect", "--generate-shell-completion"}))
	assert.Contains(t, out.String(), "pod:")
	assert.Contains(t, out.String(), "po:")
	assert.Contains(t, out.String(), "deploy:")
}

// TestInspectAliasPodDispatchesToPod pins that the "po" alias on the inspect
// pod leaf routes to inspect pod: help output proves urfave dispatched via
// the alias, not an unknown-command error.
func TestInspectAliasPodDispatchesToPod(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(context.Background(), []string{"rancher", "inspect", "po", "--help"}))
	assert.Contains(t, out.String(), "Inspect a pod")
}

// TestInspectAliasDeployDispatchesToDeployment pins that the "deploy" alias on
// the inspect deployment leaf routes to inspect deployment.
func TestInspectAliasDeployDispatchesToDeployment(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(context.Background(), []string{"rancher", "inspect", "deploy", "--help"}))
	assert.Contains(t, out.String(), "Inspect a deployment")
}

// TestRoleValueSiteFullCallback is the real-BuildRoot v2-protocol layer (c)
// from the role-completion spec: exercises space-form, equals-form, and
// network-gate-off paths through the actual root.Run callback.
// NOT parallel: t.Setenv mutates env globals, setStoredConfig mutates storedConfig.
func TestRoleValueSiteFullCallback(t *testing.T) {
	srv, _ := roleServer(t)

	setup := func(t *testing.T, networkOn bool) {
		t.Helper()
		dir := t.TempDir()
		cfg := &config.Config{
			CurrentContext: "ctx",
			Contexts:       map[string]config.Context{"ctx": {Server: "srv"}},
			Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:     config.CompletionConfig{Network: networkOn},
		}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		setStoredConfig(cfg, dir)
		t.Setenv(config.EnvConfigDir, dir)
		t.Setenv(config.EnvToken, "mgmt-token")
		t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	}

	run := func(t *testing.T, words []string, currentWord string) string {
		t.Helper()
		scriptArgs := append([]string{"--rancher-completion-proto=2"}, words...)
		scriptArgs = append(scriptArgs, "--rancher-completion-cur="+currentWord, "--generate-shell-completion")
		CaptureArgv(scriptArgs)
		root, err := BuildRoot("test")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer = &out
		_ = root.Run(context.Background(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
		return out.String()
	}

	t.Run("space-form --global --role → globalroles", func(t *testing.T) {
		setup(t, true)
		got := run(t, []string{"grant", "jane", "--global", "--role"}, "")
		assert.Contains(t, got, "admin")
		assert.Contains(t, got, "user")
	})

	t.Run("equals-form --global --role= → --role=<value> candidates", func(t *testing.T) {
		setup(t, true)
		got := run(t, []string{"grant", "jane", "--global"}, "--role=")
		assert.Contains(t, got, "--role=admin")
		assert.Contains(t, got, "--role=user")
	})

	t.Run("network gate off → empty", func(t *testing.T) {
		setup(t, false)
		got := run(t, []string{"grant", "jane", "--global", "--role"}, "")
		assert.Empty(t, got)
	})
}
