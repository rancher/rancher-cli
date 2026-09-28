package configcmd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// raceDir is a directory declared decl whose entry prod, at url, holds
// alice's credential, stored by a login.
func raceDir(t *testing.T, url, decl, pin string, login func(*config.PersistLoginInput)) string {
	t.Helper()
	t.Setenv(config.EnvCredentialStore, "")
	keyring.MockInit()
	config.ResetKeyringUsableCacheForTest(t)
	dir := t.TempDir()
	cfg := &config.Config{CredentialStore: decl, CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: url}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	if pin != "" {
		require.NoError(t, config.WritePin(dir, pin))
	}
	_, err := loginProd(t, dir, url, "alice", login)
	require.NoError(t, err)
	return dir
}

// loginProd is `rancher auth login prod` as user through the login
// primitive.
func loginProd(t *testing.T, dir, url, user string, change func(*config.PersistLoginInput)) (config.PersistLoginOutcome, error) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	in := config.PersistLoginInput{
		Name: "prod", CanonicalURL: url, Bearer: user, Provenance: clierr.ProvenanceDerived,
		ObservedDeclaration: cfg.CredentialStore,
		OpenStore:           config.OpenResolvedStore,
	}
	if change != nil {
		change(&in)
	}
	return config.PersistLoginCredential(t.Context(), dir, in)
}

// swapFile replaces path with a directory, which no read parses and no
// atomic rename can replace, and returns the undo that puts the file back
// as it was when it was swapped.
func swapFile(t *testing.T, path string) (restore func()) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	return func() {
		require.NoError(t, os.Remove(path))
		require.NoError(t, os.WriteFile(path, data, 0o600))
	}
}

// TestAnUpdateServerRacingACacheWriterLeavesNoRecordValid asserts the
// window `config update-server --url` opens with its first write: a
// kubectl call that loads the config right after it and reads the
// credential before the delete writes its record after the purge and the
// entry write. The record is keyed by the address the call used, which
// is the old one, and must never be served for it.
func TestAnUpdateServerRacingACacheWriterLeavesNoRecordValid(t *testing.T) {
	m := testrancher.NewMadeFrom()
	t.Cleanup(m.Close)
	dir := raceDir(t, m.URL, "file", "", nil)

	race := cmdtest.NewRace(t)
	m.During(race.Hold)
	var started atomic.Bool
	restore := config.SetAfterWriteHookForTest(func(what string) {
		if what != "config.yaml" || started.Swap(true) {
			return
		}
		race.Start(t, dir, m.URL, "prod")
		race.WaitHeld(t)
	})
	_, err := newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"url": "https://moved.example.com"}, Pos: []string{"prod"}}, nil)
	restore()
	race.Release()
	require.NoError(t, err)
	race.Wait(t)

	tok, _ := cmdtest.Kubectl(t, dir, m.URL, "prod")
	assert.NotEqual(t, testrancher.MadeFromToken("alice"), tok,
		"a kubectl call for the old address is never served alice's token once her credential is cleared")
}

// TestADeleteServerRacingACacheWriterLeavesNoRecordValid asserts the
// window `config delete-server` leaves across its purge: a kubectl call
// that read the credential before the delete writes its record after the
// purge, and the write that would remove the entry fails, as it does for
// a process killed before it. The entry stays behind with no credential;
// once another config write puts the binding stamp back in step, the
// record must still not be valid for it.
func TestADeleteServerRacingACacheWriterLeavesNoRecordValid(t *testing.T) {
	m := testrancher.NewMadeFrom()
	t.Cleanup(m.Close)
	dir := raceDir(t, m.URL, "file", "", nil)
	// A record for the server the purge has to remove, so the purge
	// writes the cache file and the moment after it can be seen.
	_, err := cmdtest.Kubectl(t, dir, m.URL, "another-slot")
	require.NoError(t, err)

	race := cmdtest.NewRace(t)
	m.During(race.Hold)
	race.Start(t, dir, m.URL, "prod")
	race.WaitHeld(t)

	var deleted, purged atomic.Bool
	var restoreConfig func()
	restore := config.SetAfterWriteHookForTest(func(what string) {
		switch {
		case what == "credentials.yaml":
			deleted.Store(true)
		case what == "token-cache.yaml" && deleted.Load() && !purged.Swap(true):
			// The purge holds the cache lock here: the writer is let go,
			// and writes its record once the purge is done.
			race.Release()
			restoreConfig = swapFile(t, config.FilePath(dir))
		}
	})
	_, err = NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	race.Wait(t)
	restore()
	require.Error(t, err, "the write that removes the entry failed")
	require.True(t, purged.Load())
	restoreConfig()
	_, gerr := config.NewFileStore(dir).GetToken(config.ServerRef{Name: "prod", URL: m.URL})
	require.ErrorIs(t, gerr, config.ErrNoToken, "prod's credential is gone")
	// The next config write that stamps puts the directory's two halves
	// back in step, as any login of another entry would.
	addServerForUpdateTests(t, dir, "staging", "https://staging.example.com")

	tok, _ := cmdtest.Kubectl(t, dir, m.URL, "prod")
	assert.NotEqual(t, testrancher.MadeFromToken("alice"), tok,
		"with the entry left behind, a kubectl call for prod is never served alice's token")
}

// TestACAOnlyUpdateDuringAStoreChangeKeepsTheChangeInEffect asserts that a
// config write which changes no credential leaves a change in progress
// as it found it: a pinned change to the keychain whose old copy and pin
// both could not be written has both steps left, `config update-server
// --cacert` rewrites the entry, a kubectl call runs while readers still
// resolve the old copy, and another command's prelude later finishes the
// change without writing config.yaml.
func TestACAOnlyUpdateDuringAStoreChangeKeepsTheChangeInEffect(t *testing.T) {
	m := testrancher.NewMadeFrom()
	t.Cleanup(m.Close)
	dir := raceDir(t, m.URL, "auto", "file", func(in *config.PersistLoginInput) { in.Policy = "file" })

	var restores []func()
	restoreHook := config.SetPersistWriteHookForTest(func(step string) error {
		if step == "transition" {
			restores = append(restores, swapFile(t, config.CredentialsFilePath(dir)), swapFile(t, config.PinPath(dir)))
		}
		return nil
	})
	_, err := loginProd(t, dir, m.URL, "bob", func(in *config.PersistLoginInput) { in.Policy = "auto" })
	restoreHook()
	require.ErrorContains(t, err, "neither the old copy nor the credential store pin could be changed")
	for _, r := range restores {
		r()
	}

	_, err = newUpdateServerCmd(dir).Execute(t.Context(), cmdtest.Input{Str: map[string]string{"cacert": selfSignedPEM(t)}, Pos: []string{"prod"}}, nil)
	require.NoError(t, err)

	_, err = cmdtest.Kubectl(t, dir, m.URL, "prod")
	require.NoError(t, err)

	require.NoError(t, config.WithLock(t.Context(), dir, func() error {
		cfg, lerr := config.Load(config.FilePath(dir))
		if lerr != nil {
			return lerr
		}
		_, perr := config.PrepareMutation(t.Context(), dir, cfg, config.MutationIntent{})
		return perr
	}))
	require.Equal(t, "keychain", config.ReadPin(dir), "the replay moved readers to the keychain")

	tok, _ := cmdtest.Kubectl(t, dir, m.URL, "prod")
	assert.NotEqual(t, testrancher.MadeFromToken("alice"), tok, "alice's token is not served while prod resolves bob")
}

// selfSignedPEM is a certificate that parses, for a CA-only update.
func selfSignedPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// TestAnAddedEntryNeverServesARecordAnEarlierEntryLeft asserts that an
// entry `config add-server` creates starts with a fresh cache binding id:
// a record an earlier entry of the same name left behind, made before
// entries carried an id, must not become the new entry's.
func TestAnAddedEntryNeverServesARecordAnEarlierEntryLeft(t *testing.T) {
	m := testrancher.NewMadeFrom()
	t.Cleanup(m.Close)
	dir := raceDir(t, m.URL, "file", "", nil)
	// The earlier entry of the same name, as a binary that predates the
	// id left it: no id on the entry, and a record made from it.
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	e := cfg.Servers["prod"]
	e.CacheBindingID = ""
	cfg.Servers["prod"] = e
	require.NoError(t, cfg.Write())
	_, err = cmdtest.Kubectl(t, dir, m.URL, "prod")
	require.NoError(t, err)
	cfg, err = config.Load(config.FilePath(dir))
	require.NoError(t, err)
	delete(cfg.Servers, "prod")
	cfg.CurrentServer = ""
	require.NoError(t, cfg.Write())
	require.NoError(t, config.NewFileStore(dir).DeleteToken(config.ServerRef{Name: "prod", URL: m.URL}))

	addServerForUpdateTests(t, dir, "prod", m.URL)

	tok, _ := cmdtest.Kubectl(t, dir, m.URL, "prod")
	assert.NotEqual(t, testrancher.MadeFromToken("alice"), tok, "the new entry is not served the record the earlier one left")
}
