package configcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
)

// runDeleteServer runs `config delete-server <name>` with the production delete.
// Errors are reported via assert.
func runDeleteServer(t *testing.T, dir, name string) {
	t.Helper()
	cmd := &configDeleteServerCmd{dir: dir, delete: config.DeleteTokenEverywhere}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{name}}, ui.Discard())
	assert.NoError(t, err)
}

// runUseServer runs `config use-server <name>`. Errors are reported via assert.
func runUseServer(t *testing.T, dir, name string) {
	t.Helper()
	cmd := &serverSwitchCmd{dir: dir}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{name}}, ui.Discard())
	assert.NoError(t, err)
}

// runSetCompletion runs `config set-completion` with the given network and/or
// timeout flags. Errors are reported via assert.
func runSetCompletion(t *testing.T, dir, network, timeout string) {
	t.Helper()
	cmd := &setCompletionCmd{dir: dir}
	a := cmdtest.Input{Str: map[string]string{}}
	if network != "" {
		a.Str["network"] = network
	}
	if timeout != "" {
		a.Str["timeout"] = timeout
	}
	_, err := cmd.Execute(t.Context(), a, ui.Discard())
	assert.NoError(t, err)
}

func TestDeleteServerSerializedUnderConfigLock(t *testing.T) {
	dir := cmdtest.SeedTwoServerConfig(t)
	cmdtest.AssertSerializedWithLoadUnderLock(t, dir,
		func() { runDeleteServer(t, dir, "acme") },
		func() { cmdtest.WriteSentinelServer(t, dir) },
		func() { runDeleteServer(t, dir, "acme2") },
	)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["sentinel"]
	assert.True(t, ok, "sentinel committed by mutateWhileHeld must survive both deletes")
	_, ok = cfg.Servers["acme"]
	assert.False(t, ok, "acme must be absent after delete-server")
	_, ok = cfg.Servers["acme2"]
	assert.False(t, ok, "acme2 must be absent after delete-server")
}

func TestUseServerSerializedUnderConfigLock(t *testing.T) {
	dir := cmdtest.SeedTwoServerConfig(t)
	// first switches to acme, second switches to acme2; final current server
	// must be acme2 (order proof: the second's write always wins).
	cmdtest.AssertSerializedWithLoadUnderLock(t, dir,
		func() { runUseServer(t, dir, "acme") },
		func() { cmdtest.WriteSentinelServer(t, dir) },
		func() { runUseServer(t, dir, "acme2") },
	)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["sentinel"]
	assert.True(t, ok, "sentinel committed by mutateWhileHeld must survive")
	assert.Equal(t, "acme2", cfg.CurrentServer, "current server must be the second command's target")
}

func TestSetCompletionSerializedUnderConfigLock(t *testing.T) {
	dir := cmdtest.SeedTwoServerConfig(t)
	// first enables network, second sets timeout; both fields must survive
	// (non-clobbering proof: second loads first's write and preserves network).
	cmdtest.AssertSerializedWithLoadUnderLock(t, dir,
		func() { runSetCompletion(t, dir, "true", "") },
		func() { cmdtest.WriteSentinelServer(t, dir) },
		func() { runSetCompletion(t, dir, "", "1s") },
	)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["sentinel"]
	assert.True(t, ok, "sentinel committed by mutateWhileHeld must survive")
	assert.True(t, cfg.Completion.Network, "network enabled by first command must survive second command's write")
	assert.Equal(t, "1s", cfg.Completion.Timeout, "timeout set by second command must be present")
}
