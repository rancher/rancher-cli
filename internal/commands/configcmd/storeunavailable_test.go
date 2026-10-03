package configcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
)

// keychainDir writes a directory that keeps its credentials in the
// keychain, with no entries yet.
func keychainDir(t *testing.T) string {
	t.Helper()
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	dir := t.TempDir()
	cfg := &config.Config{CredentialStore: "keychain", Servers: map[string]config.Server{}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir
}

// assertUnavailableEnvelope asserts the envelope an agent decodes from err
// is CREDENTIAL_STORE_UNAVAILABLE, exit 14, retryable, with no hint, and
// that its message carries every want.
func assertUnavailableEnvelope(t *testing.T, err error, want ...string) {
	t.Helper()
	got, exit := cmdtest.RenderedError(t, err)
	assert.Equal(t, clierr.CodeCredentialStoreUnavailable, got.Code)
	assert.Equal(t, 14, exit)
	assert.True(t, got.Retryable)
	assert.Empty(t, got.Hint)
	for _, w := range want {
		assert.Contains(t, got.Message, w)
	}
}

// TestUpdateServerURLChangeFailsAsStoreUnavailable asserts the URL change
// whose credential purge cannot reach the keychain: it fails as
// CREDENTIAL_STORE_UNAVAILABLE, and the entry keeps its old address.
func TestUpdateServerURLChangeFailsAsStoreUnavailable(t *testing.T) {
	dir := keychainDir(t)
	addServerForUpdateTests(t, dir, "qa1", "https://qa1.ex")
	cmdtest.KeychainCopy(t, dir, "qa1", "tok")
	t.Cleanup(config.SetKeyringBackendForTest(cmdtest.FailingKeychain{Err: cmdtest.SecurityExitError(t, 128), FailGet: true}))

	_, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://new.ex"}, Pos: []string{"qa1"}}, nil)
	assertUnavailableEnvelope(t, err, `failed to clear credentials for "qa1"`, "the keychain could not read the credential for 'qa1'", "then run this command again")
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.Equal(t, "https://qa1.ex", cfg.Servers["qa1"].URL)
}

// TestAddServerOrphanRemovalFailsAsStoreUnavailable asserts the orphan
// removal before an entry is written: an orphan in a keychain that cannot
// be read, or held by a helper that fails, refuses the command as
// CREDENTIAL_STORE_UNAVAILABLE and no entry is written.
func TestAddServerOrphanRemovalFailsAsStoreUnavailable(t *testing.T) {
	t.Run("the keychain", func(t *testing.T) {
		dir := keychainDir(t)
		cmdtest.KeychainCopy(t, dir, "orphan", "left-behind")
		t.Cleanup(config.SetKeyringBackendForTest(cmdtest.FailingKeychain{Err: cmdtest.SecurityExitError(t, 128), FailGet: true}))

		_, err := newAddServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://orphan.ex"}, Pos: []string{"orphan"}}, nil)
		assertUnavailableEnvelope(t, err,
			"a credential stored under 'orphan' that no entry names is in the keychain and has to be removed before this command writes the entry: the keychain could not read the credential for 'orphan'",
			"then run this command again, or use a different --config")
		cfg, lerr := config.Load(config.FilePath(dir))
		require.NoError(t, lerr)
		assert.NotContains(t, cfg.Servers, "orphan")
	})

	t.Run("a credential helper", func(t *testing.T) {
		dir, _ := cmdtest.SeedExecLogout(t, "", "orphan")
		cfg, err := config.Load(config.FilePath(dir))
		require.NoError(t, err)
		delete(cfg.Servers, "orphan")
		require.NoError(t, cfg.Write())
		t.Setenv("FAKEHELPER_MODE", "fail")

		_, err = newAddServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://orphan.ex"}, Pos: []string{"orphan"}}, nil)
		assertUnavailableEnvelope(t, err,
			"a credential stored under 'orphan' that no entry names is held by the credential helper fake and has to be erased before this command writes the entry: credential helper rancher-credential-fake could not erase the credential for 'orphan': fakehelper: deliberate failure")
		after, lerr := config.Load(config.FilePath(dir))
		require.NoError(t, lerr)
		assert.NotContains(t, after.Servers, "orphan")
	})
}
