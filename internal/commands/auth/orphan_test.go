package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
)

// orphanDir is a directory with one entry and nothing under "acme" in
// config.yaml.
func orphanDir(t *testing.T) string {
	t.Helper()
	t.Setenv(config.EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{CredentialStore: "file", Servers: map[string]config.Server{"prod": {URL: "https://prod.ex"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir
}

func loginWithURL(t *testing.T, dir string, reached *bool) (map[string]any, []string, error) {
	t.Helper()
	cmd := &loginCmd{dir: dir, newStore: config.OpenResolvedStore}
	cmd.verify = func(context.Context, config.Effective) (string, error) { *reached = true; return "", nil }
	res, err := cmd.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://acme.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"acme"}},
		cmdtest.StdinPrompter("id:sec\n"))
	if err != nil {
		return nil, nil, err
	}
	data, _ := res.Data.(map[string]any)
	return data, res.Warnings, nil
}

// TestALoginRegisteringANewNameRemovesACredentialLeftUnderIt asserts what
// `auth login --url` does with a credential stored under the name it is
// about to register when config.yaml has no such entry: it writes the
// entry before the sign-in, so it removes that credential first, under the
// same lock, and says so in its warnings. Left beside the new entry, it
// would count as the entry's: an unpinned directory would read as keeping
// credentials in both stores until a logout.
func TestALoginRegisteringANewNameRemovesACredentialLeftUnderIt(t *testing.T) {
	dir := orphanDir(t)
	ks, _, err := config.OpenResolvedStore("keychain", dir)
	require.NoError(t, err)
	require.NoError(t, ks.SetToken(config.ServerRef{Name: "acme", URL: "https://acme.ex"}, "acme-orphan-keychain"))
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "acme"}, "acme-orphan-file"))

	reached := false
	_, warnings, err := loginWithURL(t, dir, &reached)
	require.NoError(t, err)
	assert.True(t, reached)
	joined := ""
	for _, w := range warnings {
		joined += w + "\n"
	}
	assert.Contains(t, joined, "removed a credential stored under acme that no entry named, from the keychain")
	assert.Contains(t, joined, "removed a credential stored under acme that no entry named, from the plaintext file "+config.CredentialsFilePath(dir))
	ids, ierr := config.IdentityEntries(dir)
	require.NoError(t, ierr)
	assert.NotContains(t, ids, "acme", "the keychain copy and its record are gone")
	tok, gerr := config.NewFileStore(dir).GetToken(config.ServerRef{Name: "acme"})
	require.NoError(t, gerr)
	assert.Equal(t, "id:sec", tok, "and the file store holds the new login's credential")
}

// TestALoginRegisteringANewNameRefusesAnOrphanInAnUnreachableKeychain
// asserts the one case the removal cannot make: an orphan under the name in
// a keychain this process cannot reach. The login is refused before the
// sign-in and before the entry is written, with the way out.
func TestALoginRegisteringANewNameRefusesAnOrphanInAnUnreachableKeychain(t *testing.T) {
	dir := orphanDir(t)
	ks, _, err := config.OpenResolvedStore("keychain", dir)
	require.NoError(t, err)
	require.NoError(t, ks.SetToken(config.ServerRef{Name: "acme", URL: "https://acme.ex"}, "acme-orphan-keychain"))
	keyring.MockInitWithError(errors.New("dbus: no session bus"))
	t.Cleanup(keyring.MockInit)

	reached := false
	_, _, err = loginWithURL(t, dir, &reached)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "make the keychain reachable, or use a different --config")
	assert.False(t, reached, "refused before the sign-in")
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.NotContains(t, cfg.Servers, "acme", "and before the entry was written")
}

// TestAnAbandonedURLLoginLeavesNoHelperOrphanToServe asserts the removal
// reaches the credential helper a directory reads from on the `--url`
// path. The registration writes the entry before the sign-in; a helper
// credential left under that name and URL - recorded, from an earlier
// entry of that name - would be served to every reader of the new entry,
// and for as long as the login stays abandoned. The registration erases it
// through the helper first and says so.
func TestAnAbandonedURLLoginLeavesNoHelperOrphanToServe(t *testing.T) {
	t.Setenv("PATH", cmdtest.BuildFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEHELPER_MODE", "ok")
	t.Setenv("FAKEHELPER_STATE", filepath.Join(t.TempDir(), "state.json"))
	t.Setenv(config.EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{CredentialStore: "exec:fake", Servers: map[string]config.Server{"prod": {URL: "https://prod.ex"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	hs, _, err := config.OpenResolvedStore("exec:fake", dir)
	require.NoError(t, err)
	require.NoError(t, hs.SetToken(config.ServerRef{Name: "acme", URL: "https://acme.ex"}, "acme-orphan-helper"))

	cmd := &loginCmd{dir: dir, newStore: config.OpenResolvedStore}
	cmd.verify = func(context.Context, config.Effective) (string, error) { return "", errors.New("server unreachable") }
	_, lerr := cmd.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://acme.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"acme"}},
		cmdtest.StdinPrompter("id:sec\n"))
	require.Error(t, lerr, "the login is abandoned after its registration")

	after, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	require.Contains(t, after.Servers, "acme", "the registration wrote the entry")
	store, err := config.OpenStoreFor(after, dir)
	require.NoError(t, err)
	_, gerr := store.GetToken(config.ServerRef{Name: "acme", URL: after.Servers["acme"].URL})
	require.ErrorIs(t, gerr, config.ErrNoToken, "a reader of acme is not served the credential no entry named")
}

// TestARefusedURLLoginStillSaysWhatItsRegistrationRemoved asserts that no
// removal is silent: the registration removes a credential left under the
// new name before the store-change check runs, and when that check then
// refuses the login, the refusal carries the removal's notice.
func TestARefusedURLLoginStillSaysWhatItsRegistrationRemoved(t *testing.T) {
	dir := orphanDir(t)
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "prod", URL: "https://prod.ex"}, "prod-tok"))
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "acme"}, "acme-orphan-file"))

	cmd := &loginCmd{dir: dir, newStore: config.OpenResolvedStore}
	reached := false
	cmd.verify = func(context.Context, config.Effective) (string, error) { reached = true; return "", nil }
	_, err := cmd.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://acme.ex", "credential-store": "keychain"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"acme"}},
		cmdtest.StdinPrompter("id:sec\n"))
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, err, "the credential of prod is still in the plaintext file")
	assert.False(t, reached)
	assert.Contains(t, ce.Warnings(), "removed a credential stored under acme that no entry named, from the plaintext file "+config.CredentialsFilePath(dir))
}

// TestAnAbandonedURLLoginKeepsAnExistingEntrysCredential asserts the
// removal never reaches a configured entry: `auth login prod --url` for an
// entry that is already registered at that URL and logged in writes no new
// entry, removes nothing, and a login abandoned after it leaves prod's
// credential where it was.
func TestAnAbandonedURLLoginKeepsAnExistingEntrysCredential(t *testing.T) {
	dir := orphanDir(t)
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "prod", URL: "https://prod.ex"}, "prod-tok"))

	cmd := &loginCmd{dir: dir, newStore: config.OpenResolvedStore}
	cmd.verify = func(context.Context, config.Effective) (string, error) { return "", errors.New("server unreachable") }
	_, err := cmd.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://prod.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}},
		cmdtest.StdinPrompter("id:sec\n"))
	require.Error(t, err, "the login is abandoned")
	tok, gerr := config.NewFileStore(dir).GetToken(config.ServerRef{Name: "prod", URL: "https://prod.ex"})
	require.NoError(t, gerr)
	assert.Equal(t, "prod-tok", tok, "the logged-in entry's credential survives")
}
