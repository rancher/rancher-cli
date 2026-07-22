package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// TestLazyStoreResolvesBackendExactlyOnce pins the sync.Once contract: many
// calls across many goroutines resolve the backend a single time, and every
// call observes the same backend.
func TestLazyStoreResolvesBackendExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	fs := NewFileStore(dir)
	require.NoError(t, fs.SetToken(ServerRef{Name: "prod"}, "tok"))

	var calls int
	l := &lazyStore{resolve: func() (CredStore, error) {
		calls++
		return fs, nil
	}}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := l.GetToken(ServerRef{Name: "prod"})
			assert.NoError(t, err)
			assert.Equal(t, "tok", tok)
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, calls)
}

// TestLazyStoreDeferredErrorIsStable: a failed resolution surfaces the SAME
// raw error from the first and every later call (spec R1-3: returned as-is,
// callers add their own context).
func TestLazyStoreDeferredErrorIsStable(t *testing.T) {
	boom := errors.New("writing credential store pin: disk full")
	l := &lazyStore{resolve: func() (CredStore, error) { return nil, boom }}

	_, err1 := l.GetToken(ServerRef{Name: "prod"})
	err2 := l.SetToken(ServerRef{Name: "prod"}, "tok")
	require.ErrorIs(t, err1, boom)
	require.ErrorIs(t, err2, boom)
}

// TestNewLazyStoreValidatesPolicyEagerly: an unknown effective policy fails
// at CONSTRUCTION with OpenStore's exact error text (CS-6 unchanged), and no
// filesystem state appears.
func TestNewLazyStoreValidatesPolicyEagerly(t *testing.T) {
	t.Setenv(EnvCredentialStore, "wat")
	dir := t.TempDir() + "/fresh"

	_, err := NewLazyStore("", dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown credential store "wat"`)
	_, statErr := os.Stat(dir)
	assert.True(t, os.IsNotExist(statErr))
}

// TestNewLazyStoreCapturesEffectivePolicy (spec R1-4): the policy resolved
// at construction is the policy opened, even if env changes before first
// use.
func TestNewLazyStoreCapturesEffectivePolicy(t *testing.T) {
	t.Setenv(EnvCredentialStore, "file")
	dir := t.TempDir()
	l, err := NewLazyStore("", dir)
	require.NoError(t, err)

	// Env flips to an INVALID policy after construction; the captured
	// "file" policy must still be what resolves.
	t.Setenv(EnvCredentialStore, "wat")
	require.NoError(t, l.SetToken(ServerRef{Name: "prod"}, "tok"))
	tok, err := NewFileStore(dir).GetToken(ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "tok", tok)
}

// TestNewLazyStoreNoSideEffectsBeforeFirstUse pins LS-1's core: constructing
// the store for a fresh dir creates NOTHING until a credential call happens.
func TestNewLazyStoreNoSideEffectsBeforeFirstUse(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	dir := t.TempDir() + "/fresh"

	_, err := NewLazyStore("", dir)
	require.NoError(t, err)
	_, statErr := os.Stat(dir)
	assert.True(t, os.IsNotExist(statErr), "construction must not create the config dir or pin")
}

// TestLoginChoreographyFreshAutoUsableKeychain drives login's store
// sequence (NewLazyStore → RepinForLogin → SetToken) on a fresh auto
// profile with a usable keychain: exactly one probe, keychain pin,
// exactly one first-pin notice, token in the keychain and no plaintext
// file — the lazy store never resolves (RepinForLogin returned the
// concrete upgraded store).
func TestLoginChoreographyFreshAutoUsableKeychain(t *testing.T) {
	dir := t.TempDir()
	keyring.MockInit()
	resetKeyringUsableCache()
	probes := 0
	origUsable := keyringUsable
	keyringUsable = func() bool { probes++; return true }
	t.Cleanup(func() { keyringUsable = origUsable; resetKeyringUsableCache() })

	var buf bytes.Buffer
	origOut := logrus.StandardLogger().Out
	logrus.SetOutput(&buf)
	defer logrus.SetOutput(origOut)

	lazy, err := NewLazyStore("", dir)
	require.NoError(t, err)
	store, err := RepinForLogin(dir, "", lazy)
	require.NoError(t, err)
	require.NoError(t, store.SetToken(ServerRef{Name: "prod"}, "tok"))

	assert.Equal(t, 1, probes)
	assert.Equal(t, "keychain", ReadPin(dir))
	assert.Equal(t, 1, strings.Count(buf.String(), "pinned credential store: keychain"))
	tok, err := keyring.Get(keyringService, "prod")
	require.NoError(t, err)
	assert.Equal(t, "tok", tok)
	_, statErr := os.Stat(filepath.Join(dir, "credentials.yaml"))
	assert.True(t, os.IsNotExist(statErr), "usable keychain must not produce a plaintext file")
}

// TestLoginChoreographyFreshAutoUnusableKeychain: same sequence with an
// unusable keychain. RepinForLogin returns the lazy store unchanged;
// SetToken resolves it — file pin, exactly one plaintext notice, no
// first-pin-keychain notice, token in credentials.yaml. The single
// probe count proves keyringUsableOnce's memo is shared between
// RepinForLogin and the deferred resolution.
func TestLoginChoreographyFreshAutoUnusableKeychain(t *testing.T) {
	dir := t.TempDir()
	keyring.MockInit()
	resetKeyringUsableCache()
	probes := 0
	origUsable := keyringUsable
	keyringUsable = func() bool { probes++; return false }
	t.Cleanup(func() { keyringUsable = origUsable; resetKeyringUsableCache() })

	var buf bytes.Buffer
	origOut := logrus.StandardLogger().Out
	logrus.SetOutput(&buf)
	defer logrus.SetOutput(origOut)

	lazy, err := NewLazyStore("", dir)
	require.NoError(t, err)
	store, err := RepinForLogin(dir, "", lazy)
	require.NoError(t, err)
	assert.Equal(t, lazy, store, "unusable keychain must return the input store unchanged")
	require.NoError(t, store.SetToken(ServerRef{Name: "prod"}, "tok"))

	assert.Equal(t, 1, probes)
	assert.Equal(t, "file", ReadPin(dir))
	assert.Equal(t, 1, strings.Count(buf.String(), "keychain unavailable; credentials will be stored in plaintext"))
	assert.NotContains(t, buf.String(), "pinned credential store: keychain")
	tok, err := NewFileStore(dir).GetToken(ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "tok", tok)
}
