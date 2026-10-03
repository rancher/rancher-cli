package auth

import (
	"context"
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestLoginFailsAsStoreUnavailable asserts each keychain site a piped-token
// login reaches, one row per site, with the failure macOS reports for a
// locked keychain whose prompt was cancelled: the envelope is
// CREDENTIAL_STORE_UNAVAILABLE, exit 14, retryable, with no hint, the
// message names the operation and the entry, and the login commits
// nothing - the directory still names the store it did and no plaintext
// file appears.
// Not parallel: it swaps the package-global keyring seam.
func TestLoginFailsAsStoreUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seed  func(t *testing.T, dir string)
		fail  cmdtest.FailingKeychain
		store string
		want  string
	}{
		{name: "the login's write",
			fail: cmdtest.FailingKeychain{FailSet: true},
			want: "the keychain could not store the credential for 'acme'"},
		{name: "the login's read of the credential it replaces",
			seed: func(t *testing.T, dir string) {
				t.Helper()
				cmdtest.KeychainCopy(t, dir, "acme", "old")
			},
			fail: cmdtest.FailingKeychain{FailGet: true},
			want: "the keychain could not read the credential for 'acme'"},
		{name: "a store change that has to read the store it leaves",
			seed: func(t *testing.T, dir string) {
				t.Helper()
				seedServer(t, dir, "staging", "https://staging.ex", "", false)
				cmdtest.KeychainCopy(t, dir, "staging", "staging-tok")
			},
			fail:  cmdtest.FailingKeychain{FailGet: true},
			store: "file",
			want:  "the credential store cannot be changed while the stored credential for staging cannot be read: the keychain could not read the credential for 'staging'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvCredentialStore, "")
			keyring.MockInit()
			t.Cleanup(keyring.MockInit)
			dir := t.TempDir()
			seedServer(t, dir, "acme", "https://acme.ex", "", true)
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			cfg.CredentialStore = "keychain"
			require.NoError(t, cfg.Write())
			if tc.seed != nil {
				tc.seed(t, dir)
			}
			fail := tc.fail
			fail.Err = cmdtest.SecurityExitError(t, 128)
			t.Cleanup(config.SetKeyringBackendForTest(fail))

			in := cmdtest.Input{Str: map[string]string{}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"acme"}}
			if tc.store != "" {
				in.Str["credential-store"] = tc.store
			}
			cmd := newPlainLoginCmd(dir, nil)
			cmd.newStore = config.OpenResolvedStore
			cmd.verify = func(context.Context, config.Effective) (string, error) { return "", nil }
			_, err = cmd.Execute(t.Context(), in, cmdtest.StdinPrompter("id:sec\n"))

			got, exit := cmdtest.RenderedError(t, err)
			assert.Equal(t, clierr.CodeCredentialStoreUnavailable, got.Code)
			assert.Equal(t, 14, exit)
			assert.True(t, got.Retryable)
			assert.Empty(t, got.Hint)
			assert.Contains(t, got.Message, tc.want)
			assert.Contains(t, got.Message, "then run this command again")
			after, lerr := config.Load(config.FilePath(dir))
			require.NoError(t, lerr)
			assert.Equal(t, "keychain", after.CredentialStore)
			assert.NoFileExists(t, config.CredentialsFilePath(dir))
		})
	}
}

// TestFirstAutoLoginOnALockedKeychainRefuses asserts the first login of a
// fresh `auto` directory whose keychain probe fails the way a locked macOS
// keychain with a cancelled prompt does: on macOS it refuses as
// CREDENTIAL_STORE_UNAVAILABLE before the token is checked, leaves
// config.yaml as it was, writes no credential and no pin, and names
// --credential-store file;
// elsewhere the status means nothing and the login pins the file store as
// before.
// Not parallel: it swaps the package-global keyring seam.
func TestFirstAutoLoginOnALockedKeychainRefuses(t *testing.T) {
	t.Setenv(config.EnvCredentialStore, "")
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
	config.ResetKeyringUsableCacheForTest(t)
	t.Cleanup(config.SetKeyringBackendForTest(cmdtest.FailingKeychain{Err: cmdtest.SecurityExitError(t, 128), FailSet: true}))
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{"acme": {URL: "https://r.ex"}}, CurrentServer: "acme"}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	before, err := os.ReadFile(config.FilePath(dir))
	require.NoError(t, err)

	reached := false
	cmd := &loginCmd{dir: dir, newStore: config.OpenResolvedStore}
	cmd.verify = func(context.Context, config.Effective) (string, error) { reached = true; return "", nil }
	_, err = cmd.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"acme"}},
		cmdtest.StdinPrompter("id:sec\n"))

	if runtime.GOOS != "darwin" {
		require.NoError(t, err)
		assert.Equal(t, "file", config.ReadPin(dir))
		return
	}
	got, exit := cmdtest.RenderedError(t, err)
	assert.Equal(t, clierr.CodeCredentialStoreUnavailable, got.Code)
	assert.Equal(t, 14, exit)
	assert.True(t, got.Retryable)
	assert.Empty(t, got.Hint)
	assert.Contains(t, got.Message, "the keychain is locked or its prompt was cancelled (security exited with status 128)")
	assert.Contains(t, got.Message, "unlock the keychain with security unlock-keychain, or approve the macOS prompt")
	assert.Contains(t, got.Message, "--credential-store file")
	assert.False(t, reached, "refused before the token is checked")
	assert.NoFileExists(t, config.PinPath(dir))
	assert.NoFileExists(t, config.CredentialsFilePath(dir))
	after, rerr := os.ReadFile(config.FilePath(dir))
	require.NoError(t, rerr)
	assert.Equal(t, string(before), string(after), "config.yaml is as it was")
}
