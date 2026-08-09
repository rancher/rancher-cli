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

func execConfigInfoResult(t *testing.T, dir string) runtime.Result {
	t.Helper()
	res, err := NewConfigInfo(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "ConfigInfo", res.Kind)
	return res
}

func execConfigInfo(t *testing.T, dir string) map[string]any {
	t.Helper()
	return execConfigInfoResult(t, dir).Data.(map[string]any)
}

// TestConfigInfoCompletionBlock pins the completion fields: effective timeout
// (after clamping/default), raw timeout when it diverges, and network.
// Completion fields are flat keys (completion.network, completion.timeout,
// completion.timeoutRaw) in both JSON data and text output.
// No t.Parallel(): t.Setenv.
func TestConfigInfoCompletionBlock(t *testing.T) {
	t.Run("defaults when no completion config set", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		data := execConfigInfo(t, dir)
		assert.Equal(t, false, data["completion.network"])
		assert.Equal(t, "500ms", data["completion.timeout"])
		_, hasRaw := data["completion.timeoutRaw"]
		assert.False(t, hasRaw, "completion.timeoutRaw must be absent when no config is set")
	})

	t.Run("configured network true and in-range timeout", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		cfg := &config.Config{}
		cfg.Completion.Network = true
		cfg.Completion.Timeout = "1s"
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())

		data := execConfigInfo(t, dir)
		assert.Equal(t, true, data["completion.network"])
		assert.Equal(t, "1s", data["completion.timeout"])
		_, hasRaw := data["completion.timeoutRaw"]
		assert.False(t, hasRaw, "completion.timeoutRaw must be absent when raw equals effective")
	})

	t.Run("clamped timeout reports both effective and raw", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		cfg := &config.Config{}
		cfg.Completion.Timeout = "10s"
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())

		res := execConfigInfoResult(t, dir)
		data := res.Data.(map[string]any)
		assert.Equal(t, false, data["completion.network"])
		assert.Equal(t, "2s", data["completion.timeout"])
		assert.Equal(t, "10s", data["completion.timeoutRaw"])
		// Text view must carry the same lines — a typo in the format strings
		// would pass all data assertions silently.
		assert.Contains(t, res.Text, "completion.network: false\n")
		assert.Contains(t, res.Text, "completion.timeout: 2s\n")
		assert.Contains(t, res.Text, "completion.timeoutRaw: 10s\n")
	})

	t.Run("non-canonical but equal raw suppresses timeoutRaw", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		cfg := &config.Config{}
		cfg.Completion.Timeout = "1000ms" // same as 1s after parse
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())

		res := execConfigInfoResult(t, dir)
		data := res.Data.(map[string]any)
		assert.Equal(t, "1s", data["completion.timeout"])
		_, hasRaw := data["completion.timeoutRaw"]
		assert.False(t, hasRaw, "completion.timeoutRaw must be absent when raw parses to the effective duration")
		assert.Contains(t, res.Text, "completion.network: false\n")
		assert.Contains(t, res.Text, "completion.timeout: 1s\n")
		assert.NotContains(t, res.Text, "completion.timeoutRaw")
	})

	t.Run("unparseable raw surfaces as timeoutRaw with default effective", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		cfg := &config.Config{}
		cfg.Completion.Timeout = "malformed"
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())

		res := execConfigInfoResult(t, dir)
		data := res.Data.(map[string]any)
		assert.Equal(t, "500ms", data["completion.timeout"])
		assert.Equal(t, "malformed", data["completion.timeoutRaw"])
		assert.Contains(t, res.Text, "completion.timeout: 500ms\n")
		assert.Contains(t, res.Text, "completion.timeoutRaw: malformed\n")
	})
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
