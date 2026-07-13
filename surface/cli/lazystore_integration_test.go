package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// seedDrainConfig writes a minimal config with one server/context and a
// file-store token, returning the config dir. Auto policy (env scrubbed
// by TestMain, empty config policy), NO pin — the fresh-profile shape.
func seedDrainConfig(t *testing.T) string {
	t.Helper()
	keyring.MockInit()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(
		"current-context: acme\nservers:\n  acme:\n    url: https://acme.ex\ncontexts:\n  acme:\n    server: acme\n"), 0o600))
	require.NoError(t, config.NewFileStore(dir).SetToken("acme", "id:sec"))
	t.Setenv(config.EnvConfigDir, dir)
	return dir
}

// TestConfigDeleteServerThroughRootWritesNoPin pins the actual FAIL-2
// regression at the resolveDeps level: running the real CLI's
// delete-server on a fresh auto profile must drain the token WITHOUT
// probing or writing a credstore pin. RED before the lazy-store swap
// (eager OpenStore pins on every command), GREEN after.
func TestConfigDeleteServerThroughRootWritesNoPin(t *testing.T) {
	dir := seedDrainConfig(t)

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	require.NoError(t, root.Run(context.Background(), []string{"rancher", "config", "delete-server", "acme"}))

	_, statErr := os.Stat(config.PinPath(dir))
	assert.True(t, os.IsNotExist(statErr), "delete-server must not write a credstore pin")
	_, terr := config.NewFileStore(dir).GetToken("acme")
	assert.ErrorIs(t, terr, config.ErrNoToken, "token must be drained")
}

// TestAuthLogoutThroughRootWritesNoPin: same contract for auth logout
// (spec R2-1 names both drain commands).
func TestAuthLogoutThroughRootWritesNoPin(t *testing.T) {
	dir := seedDrainConfig(t)

	root, err := BuildRoot("v-test")
	require.NoError(t, err)
	require.NoError(t, root.Run(context.Background(), []string{"rancher", "auth", "logout", "acme"}))

	_, statErr := os.Stat(config.PinPath(dir))
	assert.True(t, os.IsNotExist(statErr), "logout must not write a credstore pin")
	_, terr := config.NewFileStore(dir).GetToken("acme")
	assert.ErrorIs(t, terr, config.ErrNoToken, "token must be drained")
}
