package configcmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// addServerOver seeds an unpinned `auto` directory whose one entry keeps
// its credential in stagingStore, with a token under "qa" in
// credentials.yaml that no entry names - one another server issued - and
// runs `config add-server qa` at a different server.
func addServerOver(t *testing.T, stagingStore string) (string, runtime.Result) {
	t.Helper()
	t.Setenv(config.EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	file, keychain := map[string]string{}, map[string]string{}
	if stagingStore == "file" {
		file["staging"] = "staging-tok"
	} else {
		keychain["staging"] = "staging-tok"
	}
	cmdtest.SeedUnpinned(t, dir, file, keychain)
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "qa"}, "server-a-token"))

	res, err := (&configAddServerCmd{dir: dir}).Execute(t.Context(),
		cmdtest.Input{Pos: []string{"qa"}, Str: map[string]string{"url": "https://b.example.com"}}, ui.Discard())
	require.NoError(t, err)
	return dir, res
}

func readEntry(t *testing.T, dir, entry string) (string, error) {
	t.Helper()
	cfg, store, err := config.LoadAndStore(dir)
	require.NoError(t, err)
	return store.GetToken(config.ServerRef{Name: entry, URL: cfg.Servers[entry].URL})
}

// TestAddServerRemovesACredentialLeftUnderItsName asserts what `config
// add-server` does with a credential stored under the name it registers
// when config.yaml has no such entry. From the moment the entry is written
// that credential would count as the new entry's: a reader of the new
// entry would be served a bearer another server issued and send it to this
// one, and beside another entry's keychain credential it would make every
// entry fail with the both-stores error. So the command removes it first,
// under the config lock, and says so.
func TestAddServerRemovesACredentialLeftUnderItsName(t *testing.T) {
	t.Run("a reader of the new entry is never served it", func(t *testing.T) {
		dir, res := addServerOver(t, "file")
		_, err := readEntry(t, dir, "qa")
		require.ErrorIs(t, err, config.ErrNoToken, "qa reads as not logged in, never server A's bearer")
		assert.Contains(t, strings.Join(res.Warnings, "\n"),
			"removed a credential stored under qa that no entry named, from the plaintext file "+config.CredentialsFilePath(dir))
	})
	t.Run("the other entries still read", func(t *testing.T) {
		dir, _ := addServerOver(t, "keychain")
		tok, err := readEntry(t, dir, "staging")
		require.NoError(t, err, "no both-stores error for the entries already there")
		assert.Equal(t, "staging-tok", tok)
	})
}

// TestAddServerRefusesAnOrphanInAnUnreachableKeychain asserts the refusal
// `config add-server` shares with a login: a credential left under the name
// in a keychain this process cannot reach cannot be removed, so the entry
// is not written.
func TestAddServerRefusesAnOrphanInAnUnreachableKeychain(t *testing.T) {
	t.Setenv(config.EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	cmdtest.SeedUnpinned(t, dir, map[string]string{"staging": "staging-tok"}, nil)
	ks, _, err := config.OpenResolvedStore("keychain", dir)
	require.NoError(t, err)
	require.NoError(t, ks.SetToken(config.ServerRef{Name: "qa", URL: "https://a.example.com"}, "server-a-token"))
	keyring.MockInitWithError(errors.New("dbus: no session bus"))
	t.Cleanup(keyring.MockInit)

	_, err = (&configAddServerCmd{dir: dir}).Execute(t.Context(),
		cmdtest.Input{Pos: []string{"qa"}, Str: map[string]string{"url": "https://b.example.com"}}, ui.Discard())
	require.ErrorContains(t, err, "has to be removed before this command writes the entry: the keychain could not read the credential for 'qa'")
	require.ErrorContains(t, err, "then run this command again, or use a different --config")
	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.NotContains(t, cfg.Servers, "qa", "the entry was not written")
}
