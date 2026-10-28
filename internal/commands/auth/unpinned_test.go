package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
)

// TestAuthCheckOnAnUnpinnedDirectory asserts `auth status` on an `auto`
// directory with no pin. A directory whose credentials are in one store
// reports that store; one with credentials in both fails the way every
// credential command does - its store read is where the error lands -
// except under RANCHER_CLI_TOKEN, where it never opens the store and
// reports the error as a field instead.
func TestAuthCheckOnAnUnpinnedDirectory(t *testing.T) {
	run := func(t *testing.T, dir string) (map[string]any, string, error) {
		t.Helper()
		cfg, err := config.Load(config.FilePath(dir))
		require.NoError(t, err)
		store, serr := config.NewLazyStore(cfg, dir, false)
		require.NoError(t, serr)
		cmd := &authCheckCmd{
			dir: dir, store: store,
			preflight: func(context.Context, config.Effective) (string, error) { return "v1.31.0", nil },
		}
		res, err := cmd.Execute(t.Context(), cmdtest.Input{}, ui.Discard())
		if err != nil {
			return nil, "", err
		}
		data, ok := res.Data.(map[string]any)
		require.True(t, ok)
		cs, ok := data["credentialStore"].(map[string]any)
		require.True(t, ok)
		return cs, res.Text, nil
	}

	t.Run("file tokens are the store in use", func(t *testing.T) {
		keyring.MockInit()
		t.Setenv(config.EnvCredentialStore, "")
		t.Setenv(config.EnvToken, "id:secret")
		dir := t.TempDir()
		cmdtest.SeedUnpinned(t, dir, map[string]string{"prod": "id:prod"}, nil)
		cs, text, err := run(t, dir)
		require.NoError(t, err)
		assert.Equal(t, "file", cs["inUse"])
		assert.NotContains(t, cs, "error")
		assert.Contains(t, text, "in use: file")
	})

	t.Run("both stores fail the command", func(t *testing.T) {
		keyring.MockInit()
		t.Setenv(config.EnvCredentialStore, "")
		t.Setenv(config.EnvToken, "")
		dir := t.TempDir()
		cmdtest.SeedUnpinned(t, dir, map[string]string{"staging": "id:staging"}, map[string]string{"prod": "id:prod"})
		_, _, err := run(t, dir)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.True(t, config.IsCredentialsInBothStores(err))
	})

	t.Run("both stores are a field under an environment token", func(t *testing.T) {
		keyring.MockInit()
		t.Setenv(config.EnvCredentialStore, "")
		t.Setenv(config.EnvToken, "id:secret")
		dir := t.TempDir()
		cmdtest.SeedUnpinned(t, dir, map[string]string{"staging": "id:staging"}, map[string]string{"prod": "id:prod"})
		cs, text, err := run(t, dir)
		require.NoError(t, err)
		assert.Contains(t, cs["error"], "credentials in the keychain (prod) and in the plaintext file")
		assert.NotContains(t, cs, "inUse")
		assert.Contains(t, text, "error: this config directory has credentials in the keychain")
	})
}

// TestLogoutClearsCredentialsInBothStores asserts the remedy the
// both-stores error names works. Logout resolves by the pin alone, and
// with none it treats the keychain as the directory's store whenever an
// identity record exists, so a keychain entry's logout on an unreachable
// keychain fails loudly instead of exiting 0 with the record kept - the
// error would never clear. With the keychain reachable the same logout
// succeeds, and once one store is empty every command reads again.
func TestLogoutClearsCredentialsInBothStores(t *testing.T) {
	seed := func(t *testing.T) string {
		t.Helper()
		keyring.MockInit()
		t.Setenv(config.EnvCredentialStore, "")
		dir := t.TempDir()
		cmdtest.SeedUnpinned(t, dir, map[string]string{"staging": "id:staging"}, map[string]string{"prod": "id:prod"})
		return dir
	}
	readStaging := func(t *testing.T, dir string) (string, error) {
		t.Helper()
		cfg, store, err := config.LoadAndStore(dir, false)
		require.NoError(t, err)
		return store.GetToken(config.ServerRef{Name: "staging", URL: cfg.Servers["staging"].URL})
	}

	t.Run("an unreachable keychain fails the keychain entry's logout", func(t *testing.T) {
		dir := seed(t)
		keyring.MockInitWithError(errors.New("dbus: no session bus"))
		t.Cleanup(keyring.MockInit)

		_, err := (&logoutCmd{dir: dir}).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
		require.ErrorContains(t, err, "no session bus")
		require.ErrorContains(t, err, "make the keychain reachable, or use a different --config")

		_, rerr := readStaging(t, dir)
		assert.True(t, config.IsCredentialsInBothStores(rerr), "the state is unchanged, and still says so")
	})

	t.Run("a reachable keychain clears it", func(t *testing.T) {
		dir := seed(t)
		_, rerr := readStaging(t, dir)
		require.True(t, config.IsCredentialsInBothStores(rerr), "before: every read is refused")

		_, err := (&logoutCmd{dir: dir}).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
		require.NoError(t, err)

		tok, rerr := readStaging(t, dir)
		require.NoError(t, rerr, "one store is empty, so the other is the answer")
		assert.Equal(t, "id:staging", tok)
		assert.NoFileExists(t, config.PinPath(dir), "and nothing pinned it on the way")
	})
}
