package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// TestLazyStoreResolvesBackendExactlyOnce asserts the sync.Once contract: many
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
	for range 8 {
		wg.Go(func() {
			tok, err := l.GetToken(ServerRef{Name: "prod"})
			assert.NoError(t, err) //nolint:testifylint // runs inside wg.Go; require's FailNow must only run on the test's own goroutine
			assert.Equal(t, "tok", tok)
		})
	}
	wg.Wait()
	assert.Equal(t, 1, calls)
}

// TestLazyStoreDrainNoticesRacesFirstResolution races DrainNotices against
// the credential call that triggers first resolution: the "resolved"
// atomic.Bool exists specifically so DrainNotices can read the resolved
// backend without forcing resolution itself and without racing the write of
// l.store/l.err inside once.Do. Run under -race (make validate always does)
// to catch a torn read; the many iterations also give DrainNotices a real
// chance to land on both sides of the resolution window, not just win a
// scheduling fluke once. Whichever side it lands on, the result must be
// exactly one of two valid states — the pre-resolution empty read, or the
// fully-resolved backend's own notice — never anything else (a partial
// slice, a length mismatch, or the backend's notice with a resolution that
// somehow never finished).
func TestLazyStoreDrainNoticesRacesFirstResolution(t *testing.T) {
	for i := range 200 {
		dir := t.TempDir()
		backendNotices := &noticeBuffer{}
		backendNotices.record("backend notice")
		fs := newFileStore(dir, backendNotices)

		l := &lazyStore{notices: &noticeBuffer{}, resolve: func() (CredStore, error) {
			return fs, nil
		}}

		var wg sync.WaitGroup
		var drained []string
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = l.GetToken(ServerRef{Name: "x"}) // triggers first resolution
		}()
		go func() {
			defer wg.Done()
			drained = l.DrainNotices()
		}()
		wg.Wait()

		switch len(drained) {
		case 0:
			// DrainNotices ran (or the resolved check landed) before
			// resolution completed — the valid pre-resolution empty read.
		case 1:
			assert.Equal(t, []string{"backend notice"}, drained,
				"iteration %d: a non-empty drain must be exactly the fully-resolved backend's own notice", i)
		default:
			t.Fatalf("iteration %d: DrainNotices returned an impossible torn read: %v", i, drained)
		}
	}
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
	require.ErrorContains(t, err, `unknown credential store "wat"`)
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

// TestNewLazyStoreNoSideEffectsBeforeFirstUse asserts LS-1's core: constructing
// the store for a fresh dir creates NOTHING until a credential call happens.
func TestNewLazyStoreNoSideEffectsBeforeFirstUse(t *testing.T) {
	t.Setenv(EnvCredentialStore, "")
	dir := t.TempDir() + "/fresh"

	_, err := NewLazyStore("", dir)
	require.NoError(t, err)
	_, statErr := os.Stat(dir)
	assert.True(t, os.IsNotExist(statErr), "construction must not create the config dir or pin")
}

// TestLoginSequenceFreshAutoUsableKeychain drives a direct-call sequence
// (NewLazyStore → RepinForLogin → SetToken) on a fresh auto profile with a
// usable keychain: exactly one probe, keychain pin, first-pin notice, token in
// the keychain and no plaintext file. The lazy store never resolves because
// RepinForLogin returns the concrete upgraded store before SetToken runs.
//
// Note: this calls RepinForLogin directly on a never-pinned dir (no prior
// openStoreResolved). This exercises RepinForLogin's else branch in isolation.
// The production loginCmd.Execute path (OpenStoreWithNotice runs first, pins
// the dir, then RepinForLogin sees pin="keychain" and returns early) is covered
// by TestLoginFirstPinNoticeReachesWarnings in internal/commands/login_test.go.
func TestLoginSequenceFreshAutoUsableKeychain(t *testing.T) {
	dir := t.TempDir()
	keyring.MockInit()
	resetKeyringUsableCache()
	probes := 0
	origUsable := keyringUsable
	keyringUsable = func() bool { probes++; return true }
	t.Cleanup(func() { keyringUsable = origUsable; resetKeyringUsableCache() })

	lazy, err := NewLazyStore("", dir)
	require.NoError(t, err)
	store, pinWarning, err := RepinForLogin(dir, "", lazy)
	require.NoError(t, err)
	require.NoError(t, store.SetToken(ServerRef{Name: "prod"}, "tok"))

	assert.Equal(t, 1, probes)
	assert.Equal(t, "keychain", ReadPin(dir))
	assert.Equal(t, NoticeFirstPinKeychain, pinWarning, "expected first-pin notice from RepinForLogin")
	tok, err := keyring.Get(keyringService, "prod")
	require.NoError(t, err)
	assert.Equal(t, "tok", tok)
	_, statErr := os.Stat(filepath.Join(dir, "credentials.yaml"))
	assert.True(t, os.IsNotExist(statErr), "usable keychain must not produce a plaintext file")
}

// TestLoginSequenceFreshAutoUnusableKeychain: same sequence with an
// unusable keychain. RepinForLogin returns the lazy store unchanged;
// SetToken resolves it — file pin, exactly one plaintext notice recorded on
// the lazy store's own NoticeSource buffer (not stderr directly), no
// first-pin-keychain notice, token in credentials.yaml. The single
// probe count proves keyringUsableOnce's memo is shared between
// RepinForLogin and the deferred resolution.
func TestLoginSequenceFreshAutoUnusableKeychain(t *testing.T) {
	dir := t.TempDir()
	keyring.MockInit()
	resetKeyringUsableCache()
	probes := 0
	origUsable := keyringUsable
	keyringUsable = func() bool { probes++; return false }
	t.Cleanup(func() { keyringUsable = origUsable; resetKeyringUsableCache() })

	lazy, lazyErr := NewLazyStore("", dir)
	require.NoError(t, lazyErr)
	store, pinWarning, repinErr := RepinForLogin(dir, "", lazy)
	require.NoError(t, repinErr)
	assert.Equal(t, lazy, store, "unusable keychain must return the input store unchanged")
	assert.Empty(t, pinWarning, "no pin notice when keychain unusable")
	require.NoError(t, store.SetToken(ServerRef{Name: "prod"}, "tok"))

	notices := store.(NoticeSource).DrainNotices()

	assert.Equal(t, 1, probes)
	assert.Equal(t, "file", ReadPin(dir))
	require.Len(t, notices, 1, "must be exactly one plaintext-fallback notice")
	assert.Contains(t, notices[0], "keychain unavailable; credentials will be stored in plaintext")
	assert.NotContains(t, notices[0], "pinned credential store: keychain")
	tok, err := NewFileStore(dir).GetToken(ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "tok", tok)
}
