package commands

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func execConfigInfo(t *testing.T, dir string) map[string]any {
	t.Helper()
	res, err := NewConfigInfo(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "ConfigInfo", res.Kind)
	return res.Data.(map[string]any)
}

// TestConfigInfoEffectiveStoreSources pins the env > config > default
// resolution the command exists to make visible (vault-helper exercise
// S-05: an env-supplied store is invisible in config.yaml and get-servers).
// No t.Parallel(): t.Setenv.
func TestConfigInfoEffectiveStoreSources(t *testing.T) {
	t.Run("env wins and is named as the source", func(t *testing.T) {
		dir := t.TempDir()
		cfg := &config.Config{CredentialStore: "file"}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		t.Setenv(config.EnvCredentialStore, "exec:vault")

		data := execConfigInfo(t, dir)
		assert.Equal(t, "exec:vault", data["credentialStore"])
		assert.Equal(t, "env", data["credentialStoreSource"])
		assert.Equal(t, true, data["exists"])
	})

	t.Run("config file value with no env", func(t *testing.T) {
		dir := t.TempDir()
		cfg := &config.Config{CredentialStore: "file"}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		t.Setenv(config.EnvCredentialStore, "")

		data := execConfigInfo(t, dir)
		assert.Equal(t, "file", data["credentialStore"])
		assert.Equal(t, "config", data["credentialStoreSource"])
	})

	t.Run("default auto with pin, missing config file", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		require.NoError(t, config.WritePin(dir, "file"))

		data := execConfigInfo(t, dir)
		assert.Equal(t, "auto", data["credentialStore"])
		assert.Equal(t, "default", data["credentialStoreSource"])
		assert.Equal(t, "file", data["pin"])
		assert.Equal(t, false, data["exists"])
		assert.Equal(t, config.FilePath(dir), data["path"])
	})

	t.Run("explicit policy hides a stale auto pin", func(t *testing.T) {
		dir := t.TempDir()
		cfg := &config.Config{CredentialStore: "keychain"}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		require.NoError(t, config.WritePin(dir, "file"))
		t.Setenv(config.EnvCredentialStore, "")

		data := execConfigInfo(t, dir)
		assert.Equal(t, "keychain", data["credentialStore"])
		_, ok := data["pin"]
		assert.False(t, ok, "a stale auto pin must not be reported under an explicit policy")
	})

	t.Run("env-supplied auto still reports the pin", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "auto")
		require.NoError(t, config.WritePin(dir, "keychain"))

		data := execConfigInfo(t, dir)
		assert.Equal(t, "auto", data["credentialStore"])
		assert.Equal(t, "env", data["credentialStoreSource"])
		assert.Equal(t, "keychain", data["pin"])
	})

	t.Run("no pin omits the field", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		data := execConfigInfo(t, dir)
		_, ok := data["pin"]
		assert.False(t, ok, "absent pin must not appear as an empty field")
	})
}
