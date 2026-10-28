package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// TestKeyringServiceName asserts the OS keychain service name is the
// product identifier cli.rancher.io.
func TestKeyringServiceName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "cli.rancher.io", keyringService)
}

// TestCredentialUnderTheOldKeychainServiceReadsAsLoggedOut asserts a
// credential stored under the service name earlier versions used is not
// found: the entry reads as logged out, and a new login stores and reads
// back through the current service.
func TestCredentialUnderTheOldKeychainServiceReadsAsLoggedOut(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &Config{CurrentServer: "prod", Servers: map[string]Server{"prod": {URL: "https://r.ex"}}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())

	id, err := newKeychainAccountID()
	require.NoError(t, err)
	const token = "id:secret"
	require.NoError(t, writeIdentityRecord(dir, identityRecord{
		Version: identityVersion, Entry: "prod", State: identityActive,
		ID: id, Fingerprint: credentialFingerprint(id, token),
	}))
	require.NoError(t, keyring.Set("rancher-cli", keychainAccount(id, "prod"), token))

	store, _, err := OpenResolvedStore(backendKeychain, dir)
	require.NoError(t, err)
	ref := ServerRef{Name: "prod", URL: "https://r.ex"}

	_, err = store.GetToken(ref)
	require.ErrorIs(t, err, ErrNoToken)
	eff, err := cfg.Effective(store)
	require.NoError(t, err)
	assert.Empty(t, eff.Token)

	require.NoError(t, store.SetToken(ref, "id:new"))
	got, err := store.GetToken(ref)
	require.NoError(t, err)
	assert.Equal(t, "id:new", got)
}

// serviceRecorder is a keychain backend that records the service of every
// call and forwards it to the mock keychain.
type serviceRecorder struct{ calls []string }

func (r *serviceRecorder) Set(service, user, password string) error {
	r.calls = append(r.calls, "set "+service)
	return keyring.Set(service, user, password)
}

func (r *serviceRecorder) Get(service, user string) (string, error) {
	r.calls = append(r.calls, "get "+service)
	return keyring.Get(service, user)
}

func (r *serviceRecorder) Delete(service, user string) error {
	r.calls = append(r.calls, "delete "+service)
	return keyring.Delete(service, user)
}

// TestKeyringProbeUsesTheKeyringService asserts the store probe writes and
// deletes its marker under keyringService.
func TestKeyringProbeUsesTheKeyringService(t *testing.T) {
	keyring.MockInit()
	rec := &serviceRecorder{}
	t.Cleanup(SetKeyringBackendForTest(rec))

	require.NoError(t, keyringProbe())
	assert.Equal(t, []string{"set " + keyringService, "delete " + keyringService}, rec.calls)
}

// TestIdentityErrorNamesTheKeyringService asserts the remedy of a broken
// identity record names the keychain service the entries live under.
func TestIdentityErrorNamesTheKeyringService(t *testing.T) {
	t.Parallel()
	err := identityError(t.TempDir(), "prod", "unreadable")
	require.ErrorContains(t, err, "delete any cli.rancher.io keychain entry")
}
