package cli

import (
	"bytes"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetGroupCompletionListsAliases asserts the alias-aware override: `get
// <TAB>` must list each kind leaf's Aliases alongside its canonical name -
// urfave's own default listing (printCommandSuggestions) prints only the
// canonical Name, so `po<TAB>` would otherwise complete solely toward "pod".
func TestGetGroupCompletionListsAliases(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--config", dir, "get", "--generate-shell-completion"}))
	assert.Contains(t, out.String(), "pod:")
	assert.Contains(t, out.String(), "pods:")
	assert.Contains(t, out.String(), "po:")
}

// TestGetGroupCompletionFlagBranchPreserved asserts that the alias override
// only replaces the subcommand-listing branch: a "-"-prefixed current word
// still defers to urfave's own flag-suggestion behavior, not the alias
// listing. The `get` group command owns no flags itself (they live on each
// kind leaf), so upstream's own default already produces empty output here -
// the same empty result before and after this override is what "preserved"
// means; the regression this guards against is the override always
// printing aliases regardless of branch.
func TestGetGroupCompletionFlagBranchPreserved(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--config", dir, "get", "--dr", "--generate-shell-completion"}))
	assert.Empty(t, out.String(), "a flag-prefixed current word must not fall through to alias listing")
}

// TestInspectGroupCompletionHasAliasOverride asserts that inspect/pod,
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

// TestInspectGroupCompletionListsAliases asserts the alias-aware override on the
// inspect group: `inspect <TAB>` must list each leaf's Aliases alongside its
// canonical name, so "po" and "deploy" appear for the pod and deployment leaves.
func TestInspectGroupCompletionListsAliases(t *testing.T) {
	t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	dir := t.TempDir()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(t.Context(), []string{"rancher", "--config", dir, "inspect", "--generate-shell-completion"}))
	assert.Contains(t, out.String(), "pod:")
	assert.Contains(t, out.String(), "po:")
	assert.Contains(t, out.String(), "deploy:")
}

// TestInspectAliasPodDispatchesToPod asserts that the "po" alias on the inspect
// pod leaf routes to inspect pod: help output proves urfave dispatched via
// the alias, not an unknown-command error.
func TestInspectAliasPodDispatchesToPod(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(t.Context(), []string{"rancher", "inspect", "po", "--help"}))
	assert.Contains(t, out.String(), "Inspect a pod")
}

// TestInspectAliasDeployDispatchesToDeployment asserts that the "deploy" alias on
// the inspect deployment leaf routes to inspect deployment.
func TestInspectAliasDeployDispatchesToDeployment(t *testing.T) {
	t.Parallel()
	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	var out bytes.Buffer
	root.Writer = &out

	require.NoError(t, root.Run(t.Context(), []string{"rancher", "inspect", "deploy", "--help"}))
	assert.Contains(t, out.String(), "Inspect a deployment")
}

// TestSubjectPositionalSiteFullCallback is the real-BuildRoot v2-protocol layer
// for subject completion: exercises grant positional (space-form) and
// network-gate-off paths through the actual root.Run callback.
// NOT parallel: t.Setenv mutates env globals, setStoredConfig mutates storedConfig.
func TestSubjectPositionalSiteFullCallback(t *testing.T) {
	srv, _ := userServer(t)

	setup := func(t *testing.T, networkOn bool) {
		t.Helper()
		dir := t.TempDir()
		cfg := &config.Config{
			CurrentServer: "srv",
			Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:    config.CompletionConfig{Network: networkOn},
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
		origArgv, origWord, origProto, origV1 := rawArgv, rawCurrentWord, rawProtoV2, rawV1Completion
		t.Cleanup(func() {
			rawArgv = origArgv
			rawCurrentWord = origWord
			rawProtoV2 = origProto
			rawV1Completion = origV1
		})
		scriptArgs := append([]string{"--rancher-completion-proto=2"}, words...)
		scriptArgs = append(scriptArgs, "--rancher-completion-cur="+currentWord, "--generate-shell-completion")
		CaptureArgv(scriptArgs)
		root, err := BuildRoot("test")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer = &out
		_ = root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
		return out.String()
	}

	t.Run("grant positional → user candidates", func(t *testing.T) {
		setup(t, true)
		got := run(t, []string{"grant"}, "")
		assert.Contains(t, got, "u-alice")
		assert.Contains(t, got, "alice")
		assert.NotContains(t, got, "u-sys")
	})

	t.Run("network gate off → empty", func(t *testing.T) {
		setup(t, false)
		got := run(t, []string{"grant"}, "")
		assert.Empty(t, got)
	})
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
			CurrentServer: "srv",
			Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:    config.CompletionConfig{Network: networkOn},
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
		// Snapshot all three package globals CaptureArgv mutates and restore
		// them after the sub-test so -shuffle=on does not leak stale state into
		// unrelated tests (third-party review finding 2).
		origArgv, origWord, origProto, origV1 := rawArgv, rawCurrentWord, rawProtoV2, rawV1Completion
		t.Cleanup(func() {
			rawArgv = origArgv
			rawCurrentWord = origWord
			rawProtoV2 = origProto
			rawV1Completion = origV1
		})
		scriptArgs := append([]string{"--rancher-completion-proto=2"}, words...)
		scriptArgs = append(scriptArgs, "--rancher-completion-cur="+currentWord, "--generate-shell-completion")
		CaptureArgv(scriptArgs)
		root, err := BuildRoot("test")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer = &out
		_ = root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
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
