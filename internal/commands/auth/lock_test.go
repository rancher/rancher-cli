package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
)

// runLogin runs a delegated `auth login <name> --url <server>` with
// --token-stdin using an in-memory file store. Errors are reported via
// assert (safe from goroutines).
func runLogin(t *testing.T, dir, name, server string) {
	t.Helper()
	store := config.NewFileStore(dir)
	cmd := &loginCmd{
		dir:      dir,
		newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil },
	}
	_, err := cmd.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": server}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{name}},
		cmdtest.StdinPrompter("id:sec\n"))
	assert.NoError(t, err)
}

// runLogout runs `auth logout <name>`. Errors are reported via assert.
func runLogout(t *testing.T, dir, name string) {
	t.Helper()
	cmd := &logoutCmd{dir: dir}
	_, err := cmd.Execute(t.Context(), cmdtest.Input{Pos: []string{name}}, ui.Discard())
	assert.NoError(t, err)
}

func TestLogoutSerializedUnderConfigLock(t *testing.T) {
	dir := cmdtest.SeedTwoServerConfig(t)
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme", URL: "https://acme.example"}, "tok-acme"))
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme2", URL: "https://acme2.example"}, "tok-acme2"))

	cmdtest.AssertSerializedWithLoadUnderLock(t, dir,
		func() { runLogout(t, dir, "acme") },
		func() { cmdtest.WriteSentinelServer(t, dir) },
		func() { runLogout(t, dir, "acme2") },
	)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["sentinel"]
	assert.True(t, ok, "sentinel committed by mutateWhileHeld must survive both logouts")
	_, ok = cfg.Servers["acme"]
	assert.True(t, ok, "server config entry for acme must survive logout (only token is deleted)")
	_, ok = cfg.Servers["acme2"]
	assert.True(t, ok, "server config entry for acme2 must survive logout (only token is deleted)")
	_, err = store.GetToken(config.ServerRef{Name: "acme"})
	require.ErrorIs(t, err, config.ErrNoToken, "acme token must be deleted by logout")
	_, err = store.GetToken(config.ServerRef{Name: "acme2"})
	assert.ErrorIs(t, err, config.ErrNoToken, "acme2 token must be deleted by logout")
}
