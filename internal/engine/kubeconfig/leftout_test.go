package kubeconfig

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// refusedValues are values no printed command can hold exactly: a single
// quote, a right single quotation mark, a zero width space and two
// backslashes in a row.
var refusedValues = []struct{ name, value string }{
	{"a single quote", "o'b"},
	{"a right single quotation mark", "o\u2019b"},
	{"a zero width space", "o\u200bb"},
	{"two backslashes", `o\\b`},
}

// TestKubeconfigInstallHintLeavesOutTheStepsForAnEntryThatCannotBePrinted
// asserts the install hint of an exec command other than the default, for a
// server entry whose name cannot be printed, is the clause that asks to
// regenerate the kubeconfig from the entry and the note about the left-out
// command, with no command.
func TestKubeconfigInstallHintLeavesOutTheStepsForAnEntryThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for _, tc := range refusedValues {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := installHint("rancher-dev", tc.value, "c-1", "/d", recordsURL, false)
			assert.Equal(t, "rancher-dev was not found; restore it, or regenerate this kubeconfig from entry "+clierr.ShellQuote(clierr.IDText(tc.value))+"; "+clierr.LeftOutNote, got)
		})
	}
	assert.Equal(t, "rancher-dev was not found; restore it, or regenerate this kubeconfig from entry 'prod': rancher --config '/d' config use-server 'prod'; then rancher --config '/d' kubeconfig --cluster 'c-1'; selecting the entry switches the directory's current server",
		installHint("rancher-dev", "prod", "c-1", "/d", recordsURL, false))
}

// TestKubeconfigInstallHintLeavesOutTheCommandForAClusterThatCannotBePrinted
// asserts the install hint of a session with no entry gives no kubeconfig
// command for a cluster id that cannot be printed, and says so once.
func TestKubeconfigInstallHintLeavesOutTheCommandForAClusterThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for _, tc := range refusedValues {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := installHint("rancher-dev", "", tc.value, "/d", recordsURL, false)
			assert.NotContains(t, got, "kubeconfig --cluster")
			assert.True(t, strings.HasPrefix(got, "rancher-dev was not found; restore it, or regenerate this kubeconfig with RANCHER_CLI_URL set to "), got)
			assert.Equal(t, 1, strings.Count(got, clierr.LeftOutNote), got)
		})
	}
	assert.Contains(t, installHint("rancher-dev", "", "c-1", "/d", recordsURL, false), "and RANCHER_CLI_TOKEN set: rancher --config '/d' kubeconfig --cluster 'c-1'")
}

// TestKubeconfigNoOwnerNoticeLeavesOutTheStepsForAnEntryThatCannotBePrinted
// asserts the notice of a login that records no owner gives no login, removal
// or kubeconfig command for an entry name that cannot be printed, and says so
// once.
func TestKubeconfigNoOwnerNoticeLeavesOutTheStepsForAnEntryThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Servers: map[string]config.Server{}}
	for _, tc := range refusedValues {
		for name, plugin := range map[string]bool{"a user entry": false, "a plugin login entry": true} {
			t.Run(tc.name+", "+name, func(t *testing.T) {
				t.Parallel()
				notice := clierr.ExpandRemedies(NoOwnerNoticeForTest(cfg, tc.value, tc.value, "c-1", plugin), "")
				assert.NotContains(t, notice, "auth login")
				assert.NotContains(t, notice, "auth logout")
				assert.NotContains(t, notice, "config delete-server")
				assert.NotContains(t, notice, "kubeconfig --cluster")
				assert.Equal(t, 1, strings.Count(notice, clierr.LeftOutNote), notice)
			})
		}
	}
}

// TestKubeconfigNoStoredLoginNoticeLeavesOutTheLoginForAServerThatCannotBePrinted
// asserts the notice of a kubeconfig that no stored login answers for gives
// no login command for a server URL that cannot be printed, keeps the
// command that creates a kubeconfig with its own token, and says so once.
func TestKubeconfigNoStoredLoginNoticeLeavesOutTheLoginForAServerThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for _, tc := range refusedValues {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			notice := clierr.ExpandRemedies(NoStoredLoginNoticeForTest(&config.Config{}, "/d", "prod", "c-1", "https://"+tc.value), "")
			assert.NotContains(t, notice, "auth login")
			assert.Contains(t, notice, "or, for a kubeconfig that carries its own token, run: rancher create kubeconfig --clusters 'c-1' --apply --yes")
			assert.Equal(t, 1, strings.Count(notice, clierr.LeftOutNote), notice)
		})
	}
}

// TestKubeconfigReservedNameRefusalLeavesOutTheStepsForAClusterThatCannotBePrinted
// asserts the refusal of a --name in the plugin's cache slot namespace gives
// no steps for a cluster id that cannot be printed, and says so once.
func TestKubeconfigReservedNameRefusalLeavesOutTheStepsForAClusterThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	name := config.LegacyCachePrefix + "alice"
	for _, tc := range refusedValues {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			message := clierr.ExpandRemedies(ReservedNameRefusalForTest(name, tc.value, recordsURL), "")
			assert.NotContains(t, message, "auth login")
			assert.NotContains(t, message, "kubeconfig --cluster")
			assert.Equal(t, 1, strings.Count(message, clierr.LeftOutNote), message)
		})
	}
	message := clierr.ExpandRemedies(ReservedNameRefusalForTest(name, "c-1", recordsURL), "")
	assert.Contains(t, message, "pipe an API token to: rancher auth login <name> --url 'https://r.example.com' --token-stdin; then rancher config use-server <name>; then write this kubeconfig again with: rancher kubeconfig --cluster 'c-1'")
}
