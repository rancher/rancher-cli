package commands

import (
	"os"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func execConfigInfoResult(t *testing.T, dir string) runtime.Result {
	t.Helper()
	res, err := NewConfigInfo(dir).(runtime.ReadCommand).Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "ConfigInfo", res.Kind)
	return res
}

func execConfigInfo(t *testing.T, dir string) map[string]any {
	t.Helper()
	return execConfigInfoResult(t, dir).Data.(map[string]any)
}

// assertCompletion extracts the nested completion map from res, asserts the
// nested JSON structure, guards against both dead key shapes (flat dotted and
// camelCase), and verifies the text indentation header. Called by every
// completion sub-test so the contract holds for all cases.
func assertCompletion(t *testing.T, res runtime.Result) map[string]any {
	t.Helper()
	data := res.Data.(map[string]any)

	c, ok := data["completion"].(map[string]any)
	require.True(t, ok, "completion must be a nested map[string]any")

	// Regression guard against two real prior shapes:
	//   flat-dotted (completion.network etc.) — shipped briefly on main before
	//     the nested shape was settled;
	//   camelCase (completionNetwork etc.) — existed as a working-tree
	//     changeset on 2026-08-10, dropped before merge.
	// Both forms were concrete code, not speculation.
	for _, dead := range []string{
		"completion.network", "completion.timeout", "completion.timeoutRaw",
		"completionNetwork", "completionTimeout", "completionTimeoutRaw",
	} {
		_, present := data[dead]
		assert.False(t, present, "%q must not appear as a top-level key (nested shape required)", dead)
	}

	// Text must open the indented section.
	assert.Contains(t, res.Text, "completion:\n")

	return c
}

// TestStyleConfigInfoTextStripInvariant asserts config status's bold span: the
// "completion:" section label is bold, whole line; every flat "key: value"
// line stays plain — plus the strip invariant and the inactive-style
// identity.
func TestStyleConfigInfoTextStripInvariant(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	res := execConfigInfoResult(t, dir)
	enabled := style.Style{Enabled: true}

	styled := styleConfigInfoText(res.Text, enabled)
	assert.NotEqual(t, res.Text, styled)
	assert.Equal(t, res.Text, style.Strip(styled))
	assert.Equal(t, res.Text, styleConfigInfoText(res.Text, style.Style{}), "disabled style returns text unchanged")
	assert.Contains(t, styled, "\x1b[1mcompletion:\x1b[0m\n")
	assert.NotContains(t, styled, "\x1b[1mpath:", "a flat key: value line must not be styled")
}

// TestConfigInfoCompletionBlock asserts the completion section as a nested object
// in JSON data and an indented block in text. Both views are asserted for every
// scenario (auth-check precedent: parity is guaranteed by tests, not a shared
// loop). No t.Parallel(): t.Setenv.
func TestConfigInfoCompletionBlock(t *testing.T) {
	t.Run("defaults when no completion config set", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		res := execConfigInfoResult(t, dir)
		c := assertCompletion(t, res)

		assert.Equal(t, false, c["network"])
		assert.Equal(t, "500ms", c["timeout"])
		_, hasRaw := c["timeoutRaw"]
		assert.False(t, hasRaw, "timeoutRaw must be absent when no config is set")

		assert.Contains(t, res.Text, "  network: false\n")
		assert.Contains(t, res.Text, "  timeout: 500ms\n")
		assert.NotContains(t, res.Text, "timeoutRaw")
	})

	t.Run("configured network true and in-range timeout", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		cfg := &config.Config{}
		cfg.Completion.Network = true
		cfg.Completion.Timeout = "1s"
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())

		res := execConfigInfoResult(t, dir)
		c := assertCompletion(t, res)

		assert.Equal(t, true, c["network"])
		assert.Equal(t, "1s", c["timeout"])
		_, hasRaw := c["timeoutRaw"]
		assert.False(t, hasRaw, "timeoutRaw must be absent when raw equals effective")

		assert.Contains(t, res.Text, "  network: true\n")
		assert.Contains(t, res.Text, "  timeout: 1s\n")
		assert.NotContains(t, res.Text, "timeoutRaw")
	})

	t.Run("clamped timeout reports both effective and raw", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		cfg := &config.Config{}
		cfg.Completion.Timeout = "10s"
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())

		res := execConfigInfoResult(t, dir)
		c := assertCompletion(t, res)

		assert.Equal(t, false, c["network"])
		assert.Equal(t, "2s", c["timeout"])
		assert.Equal(t, "10s", c["timeoutRaw"])

		assert.Contains(t, res.Text, "  network: false\n")
		assert.Contains(t, res.Text, "  timeout: 2s\n")
		assert.Contains(t, res.Text, "  timeoutRaw: 10s\n")
	})

	t.Run("non-canonical but equal raw suppresses timeoutRaw", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		cfg := &config.Config{}
		cfg.Completion.Timeout = "1000ms" // same as 1s after parse
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())

		res := execConfigInfoResult(t, dir)
		c := assertCompletion(t, res)

		assert.Equal(t, "1s", c["timeout"])
		_, hasRaw := c["timeoutRaw"]
		assert.False(t, hasRaw, "timeoutRaw must be absent when raw parses to the effective duration")

		assert.Contains(t, res.Text, "  network: false\n")
		assert.Contains(t, res.Text, "  timeout: 1s\n")
		assert.NotContains(t, res.Text, "timeoutRaw")
	})

	t.Run("unparseable raw surfaces as timeoutRaw with default effective", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")
		cfg := &config.Config{}
		cfg.Completion.Timeout = "malformed"
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())

		res := execConfigInfoResult(t, dir)
		c := assertCompletion(t, res)

		assert.Equal(t, "500ms", c["timeout"])
		assert.Equal(t, "malformed", c["timeoutRaw"])

		assert.Contains(t, res.Text, "  timeout: 500ms\n")
		assert.Contains(t, res.Text, "  timeoutRaw: malformed\n")
	})
}

// TestConfigInfoEffectiveStoreSources asserts the env > config > default
// resolution the command exists to make visible (vault-helper exercise
// S-05: an env-supplied store is invisible in config.yaml and list-servers).
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
		_, ok := data["credentialStoreDefault"]
		assert.False(t, ok, "once pinned, the platform default is moot — the pin is the actual answer")
	})

	t.Run("default auto with no pin yet names the platform default, read-only", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv(config.EnvCredentialStore, "")

		data := execConfigInfo(t, dir)
		assert.Equal(t, "auto", data["credentialStore"])
		_, pinned := data["pin"]
		assert.False(t, pinned, "no pin exists yet — reporting one would imply a probe ran")
		assert.Equal(t, config.PlatformKeychainLabel(), data["credentialStoreDefault"])
		_, err := os.Stat(config.PinPath(dir))
		assert.True(t, os.IsNotExist(err), "config status must never write the pin itself")
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
