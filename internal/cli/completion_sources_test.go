package cli

import (
	"bytes"
	"testing"

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
