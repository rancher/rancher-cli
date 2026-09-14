package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

func TestFileStoreRoundTrip(t *testing.T) {
	s := NewFileStore(t.TempDir())
	t.Run("missing token", func(t *testing.T) {
		_, err := s.GetToken(ServerRef{Name: "acme"})
		assert.ErrorIs(t, err, ErrNoToken)
	})
	t.Run("set then get", func(t *testing.T) {
		require.NoError(t, s.SetToken(ServerRef{Name: "acme"}, "tok-abc:secret"))
		got, err := s.GetToken(ServerRef{Name: "acme"})
		require.NoError(t, err)
		assert.Equal(t, "tok-abc:secret", got)
	})
	t.Run("delete", func(t *testing.T) {
		require.NoError(t, s.SetToken(ServerRef{Name: "acme"}, "x"))
		require.NoError(t, s.DeleteToken(ServerRef{Name: "acme"}))
		_, err := s.GetToken(ServerRef{Name: "acme"})
		assert.ErrorIs(t, err, ErrNoToken)
	})
}

func TestOpenStoreFilePolicy(t *testing.T) {
	s, err := OpenStore("file", t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.SetToken(ServerRef{Name: "acme"}, "y"))
	got, err := s.GetToken(ServerRef{Name: "acme"})
	require.NoError(t, err)
	assert.Equal(t, "y", got)
}

func TestOpenStoreUnknownPolicyErrors(t *testing.T) {
	_, err := OpenStore("keyChain", t.TempDir())
	require.Error(t, err)
	assert.ErrorContains(t, err, "keychain, file, auto")
}

func TestOpenStoreAutoProbesOnceThenPins(t *testing.T) {
	dir := t.TempDir()
	probes := 0
	orig := keyringUsable
	keyringUsable = func() bool { probes++; return false }
	resetKeyringUsableCache() // fresh probe, not a result cached by an earlier test
	defer func() { keyringUsable = orig; resetKeyringUsableCache() }()

	if _, err := OpenStore("auto", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore("auto", dir); err != nil {
		t.Fatal(err)
	}
	// The second OpenStore call reads the pin written by the first and never
	// re-probes - this is the pin's own once-per-dir contract. probes still
	// asserts the underlying keyringUsableOnce process-wide memoization: a
	// second unpinned dir in the SAME process would reuse this cached result
	// rather than probing again (deliberate: one process, one probe).
	if probes != 1 {
		t.Errorf("probes = %d, want 1 (second run must read the pin)", probes)
	}
	if got := ReadPin(dir); got != "file" {
		t.Errorf("pin = %q, want file", got)
	}
}

func TestKeyringUsableOnceMemoizesPerProcess(t *testing.T) {
	probes := 0
	orig := keyringUsable
	keyringUsable = func() bool { probes++; return true }
	resetKeyringUsableCache()
	defer func() { keyringUsable = orig; resetKeyringUsableCache() }()

	dirA, dirB := t.TempDir(), t.TempDir()
	if _, err := OpenStore("auto", dirA); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore("auto", dirB); err != nil {
		t.Fatal(err)
	}
	if probes != 1 {
		t.Errorf("probes = %d, want 1 (a second unpinned dir in the same process must reuse the memoized result)", probes)
	}
	if got := ReadPin(dirB); got != "keychain" {
		t.Errorf("pin for dirB = %q, want keychain (memoized result still applied)", got)
	}
}

func TestKeyringUsableDegradesOnHungProbe(t *testing.T) {
	origProbe, origTimeout := keyringProbe, keyringProbeTimeout
	keyringProbe = func() bool { select {} } // never returns
	keyringProbeTimeout = 50 * time.Millisecond
	defer func() { keyringProbe, keyringProbeTimeout = origProbe, origTimeout }()

	done := make(chan bool, 1)
	go func() { done <- keyringUsable() }()

	select {
	case ok := <-done:
		assert.False(t, ok, "a hung probe must degrade to unusable")
	case <-time.After(time.Second):
		t.Fatal("keyringUsable did not honor keyringProbeTimeout")
	}
}

func TestOpenStoreExplicitPolicySkipsPin(t *testing.T) {
	dir := t.TempDir()
	if err := WritePin(dir, "keychain"); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore("file", dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(*fileStore); !ok {
		t.Errorf("explicit file policy must ignore the pin, got %T", s)
	}
}

func TestCachedCredRoundTrip(t *testing.T) {
	s := NewFileStore(t.TempDir())
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, s.SetCachedCred("acme/u/c", "tok", exp, "", "", ""))
	tok, gotExp, err := s.GetCachedCred("acme/u/c")
	require.NoError(t, err)
	assert.Equal(t, "tok", tok)
	assert.Equal(t, exp, gotExp.UTC().Truncate(time.Second))
}

func TestCachedCredExpiredIsErrNoToken(t *testing.T) {
	s := NewFileStore(t.TempDir())
	require.NoError(t, s.SetCachedCred("acme/u/c", "tok", time.Now().Add(-time.Minute), "", "", ""))
	_, _, err := s.GetCachedCred("acme/u/c")
	assert.ErrorIs(t, err, ErrNoToken)
}

func TestDeleteAllCachedCredsFlushesFileStore(t *testing.T) {
	s := NewFileStore(t.TempDir())
	require.NoError(t, s.SetCachedCred("acme/u/c1", "t1", time.Now().Add(time.Hour), "", "", ""))
	require.NoError(t, s.SetCachedCred("acme/u/c2", "t2", time.Now().Add(time.Hour), "", "", ""))
	require.NoError(t, s.DeleteAllCachedCreds())
	_, _, err := s.GetCachedCred("acme/u/c1")
	require.ErrorIs(t, err, ErrNoToken)
	_, _, err = s.GetCachedCred("acme/u/c2")
	assert.ErrorIs(t, err, ErrNoToken)
}

func TestCachedCredExpiredPurgeErrorIsPropagated(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir).(*fileStore)
	require.NoError(t, s.SetCachedCred("acme/u/c", "tok", time.Now().Add(-time.Minute), "", "", ""))

	tok, _, err := s.GetCachedCred("acme/u/c")
	require.ErrorIs(t, err, ErrNoToken)
	assert.Empty(t, tok)

	m, err := s.readCache()
	require.NoError(t, err)
	_, exists := m["acme/u/c"]
	assert.False(t, exists, "expired entry should have been removed on successful purge")
}

func TestFileStoreListCachedCreds(t *testing.T) {
	s := NewFileStore(t.TempDir())
	require.NoError(t, s.SetCachedCred("bravo", "t2", time.Now().Add(time.Hour), "", "", ""))
	require.NoError(t, s.SetCachedCred("alpha", "t1", time.Now().Add(time.Hour), "", "", ""))
	require.NoError(t, s.SetCachedCred("gone", "t3", time.Now().Add(-time.Minute), "", "", "")) // expired

	list, err := s.ListCachedCreds()
	require.NoError(t, err)
	require.Len(t, list, 2, "expired entry is pruned")
	assert.Equal(t, "alpha", list[0].Name, "sorted by name")
	assert.Equal(t, "bravo", list[1].Name)
}

func TestFileStoreWarnsOnLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("loose-permission warning is intentionally disabled on Windows (permcheck_windows.go); ACL enforcement is the guarantee there")
	}

	// Loose (0644): notices once, then deduped for the same path. The
	// notice rides the store's own NoticeSource buffer now, not stderr
	// directly.
	looseDir := t.TempDir()
	loose := NewFileStore(looseDir)
	looseNotices := loose.(NoticeSource)
	require.NoError(t, loose.SetToken(ServerRef{Name: "acme"}, "id:sec"))
	require.NoError(t, os.Chmod(filepath.Join(looseDir, "credentials.yaml"), 0o644))

	_, err := loose.GetToken(ServerRef{Name: "acme"})
	require.NoError(t, err)
	notices := looseNotices.DrainNotices()
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0], "group/world-readable", "0644 credentials must notice")

	_, err = loose.GetToken(ServerRef{Name: "acme"})
	require.NoError(t, err)
	assert.Empty(t, looseNotices.DrainNotices(), "notice must fire once per path per process")

	// Tight (0600), fresh path: silent.
	tight := NewFileStore(t.TempDir())
	require.NoError(t, tight.SetToken(ServerRef{Name: "acme"}, "id:sec")) // written 0600
	_, err = tight.GetToken(ServerRef{Name: "acme"})
	require.NoError(t, err)
	assert.Empty(t, tight.(NoticeSource).DrainNotices(), "0600 credentials must be silent")
}

func TestCacheKeyScoping(t *testing.T) {
	if got := CacheKey("prod", "rancher.example.com", "c-abc"); got != "prod@rancher.example.com/c-abc" {
		t.Errorf("CacheKey = %q", got)
	}
}

func TestScopedCacheRoundTripAndList(t *testing.T) {
	fs := NewFileStore(t.TempDir())
	key := CacheKey("prod", "r.example.com", "c-1")
	if err := fs.SetCachedCred(key, "tok", time.Now().Add(time.Hour), "prod", "r.example.com", "c-1"); err != nil {
		t.Fatal(err)
	}
	infos, err := fs.ListCachedCreds()
	if err != nil || len(infos) != 1 {
		t.Fatal(infos, err)
	}
	i := infos[0]
	if i.Name != "prod" || i.Server != "r.example.com" || i.Cluster != "c-1" || i.Key != key {
		t.Errorf("info = %+v", i)
	}
}

func TestLegacyBareEntryListsAndMisses(t *testing.T) {
	dir := t.TempDir()
	fs := &fileStore{path: filepath.Join(dir, "credentials.yaml"), cachePath: filepath.Join(dir, "token-cache.yaml")}
	// simulate a pre-change entry: bare key, no record fields
	if err := os.WriteFile(fs.cachePath, []byte("prod:\n  token: old\n  expiry: \"2027-01-01T00:00:00Z\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	infos, _ := fs.ListCachedCreds()
	if len(infos) != 1 || infos[0].Name != "prod" || infos[0].Key != "prod" {
		t.Errorf("legacy entry must list by raw key: %+v", infos)
	}
	if _, _, err := fs.GetCachedCred(CacheKey("prod", "r.example.com", "c-1")); !errors.Is(err, ErrNoToken) {
		t.Errorf("scoped read of legacy entry must miss, got %v", err)
	}
}

func TestKeyringStoreCachesToFileAndLists(t *testing.T) {
	// No keyring.MockInit needed: the exec-cred cache methods forward to the file
	// store and never touch the OS keychain backend (only GetToken/SetToken do).
	dir := t.TempDir()
	s := newPinnedKeyringStore(dir, false)
	require.NoError(t, s.SetCachedCred("x", "tok", time.Now().Add(time.Hour), "", "", ""))

	list, err := s.ListCachedCreds()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "x", list[0].Name)

	require.NoError(t, s.DeleteAllCachedCreds()) // must actually flush (was a no-op)
	list, err = s.ListCachedCreds()
	require.NoError(t, err)
	assert.Empty(t, list)
}

// storeIdentity writes an active identity record for the entry and returns
// the keychain account it names, for a test that has to set up a scoped
// credential without a working keychain.
func storeIdentity(t *testing.T, dir, entry, token string) string {
	t.Helper()
	id, err := newKeychainAccountID()
	require.NoError(t, err)
	require.NoError(t, writeIdentityRecord(dir, identityRecord{
		Version:     identityVersion,
		Entry:       entry,
		State:       identityActive,
		ID:          id,
		Fingerprint: credentialFingerprint(id, token),
	}))
	return keychainAccount(id, entry)
}

// TestKeyringGetTokenNeverReadsTheFileStore asserts that a keychain
// directory reads nothing from credentials.yaml. A token left there by an
// earlier file pin, by a copy, or by a re-login is not this directory's
// keychain credential, and serving it would be exactly the second source
// that lets a directory use a credential it never stored.
func TestKeyringGetTokenNeverReadsTheFileStore(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod"}, "id:secret"))

	s := newPinnedKeyringStore(dir, false)
	_, err := s.GetToken(ServerRef{Name: "prod"})
	require.ErrorIs(t, err, ErrNoToken, "no identity record means nothing was stored here")
}

// TestKeyringGetTokenAbsentSlotIsNotLoggedIn asserts the verdict for a
// record whose account the keychain no longer holds: logged out, not an
// error.
func TestKeyringGetTokenAbsentSlotIsNotLoggedIn(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	storeIdentity(t, dir, "prod", "gone")

	s := newPinnedKeyringStore(dir, false)
	_, err := s.GetToken(ServerRef{Name: "prod"})
	require.ErrorIs(t, err, ErrNoToken)
}

// TestKeyringGetTokenReplacedCredential asserts the verdict for a slot
// another config directory sharing this identity has written since: a
// typed error naming the remedy, never the other directory's credential.
func TestKeyringGetTokenReplacedCredential(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	account := storeIdentity(t, dir, "prod", "ours")
	require.NoError(t, keyring.Set(keyringService, account, "theirs"))

	s := newPinnedKeyringStore(dir, false)
	_, err := s.GetToken(ServerRef{Name: "prod"})
	require.ErrorContains(t, err, "was replaced by another config directory")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeCredentialReplaced, ce.Code)
	assert.False(t, ce.Retryable)
	assert.Equal(t, 3, ce.ExitCode())
}

// TestKeyringGetTokenPendingLogin asserts the verdict for a login that
// wrote its identity ahead of the credential and has not settled it.
func TestKeyringGetTokenPendingLogin(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	id, err := newKeychainAccountID()
	require.NoError(t, err)
	require.NoError(t, writeIdentityRecord(dir, identityRecord{
		Version:     identityVersion,
		Entry:       "prod",
		State:       identityPending,
		ID:          id,
		Fingerprint: credentialFingerprint(id, "in-flight"),
	}))

	s := newPinnedKeyringStore(dir, false)
	_, err = s.GetToken(ServerRef{Name: "prod"})
	require.ErrorContains(t, err, "did not complete")
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeLoginIncomplete, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Equal(t, 3, ce.ExitCode())
}

// TestKeyringGetTokenMatchingCredential asserts the one verdict that
// returns a credential: an active record whose fingerprint the slot still
// matches.
func TestKeyringGetTokenMatchingCredential(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	account := storeIdentity(t, dir, "prod", "ours")
	require.NoError(t, keyring.Set(keyringService, account, "ours"))

	s := newPinnedKeyringStore(dir, false)
	tok, err := s.GetToken(ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "ours", tok)
}

// TestDeleteTokenEverywhereRemovesFromBothBackends asserts that a
// directory holding a credential in each store loses both, and that a
// lingering identity record is reached even though the directory's own
// policy names the file store.
func TestDeleteTokenEverywhereRemovesFromBothBackends(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	account := storeIdentity(t, dir, "prod", "in-keychain")
	require.NoError(t, keyring.Set(keyringService, account, "in-keychain"))
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod"}, "in-file"))

	from, _, _, err := DeleteTokenEverywhere(t.Context(), dir, ServerRef{Name: "prod", URL: "https://prod.ex"}, "file")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"keychain", "file"}, from)

	_, gerr := keyring.Get(keyringService, account)
	require.Error(t, gerr, "the keychain account is gone")
	_, gerr = NewFileStore(dir).GetToken(ServerRef{Name: "prod"})
	require.ErrorIs(t, gerr, ErrNoToken)

	rec, rerr := readIdentityRecord(dir, "prod")
	require.NoError(t, rerr)
	assert.Nil(t, rec, "the identity record goes with the credential it named")
}

// TestDeleteLoginTokenSkipsTheKeychainWithoutARecord asserts a directory
// with no identity for the entry never reaches the keychain: nothing was
// stored there under this directory's identity, so a headless machine logs
// out of its file store cleanly.
func TestDeleteLoginTokenSkipsTheKeychainWithoutARecord(t *testing.T) {
	keyring.MockInitWithError(errors.New("keychain boom"))
	t.Cleanup(keyring.MockInit)
	dir := t.TempDir()
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod"}, "in-file"))

	from, notices, err := DeleteLoginToken(t.Context(), dir, ServerRef{Name: "prod", URL: "https://prod.ex"}, "file")
	require.NoError(t, err)
	assert.Equal(t, []string{"file"}, from)
	assert.Empty(t, notices)
	assert.NoDirExists(t, identityDirPath(dir), "a delete creates no identity directory")
}

// TestDeleteLoginTokenWarnsOnUnreachableKeychainWithALingeringRecord
// asserts the file-policy directory that still has a record: the keychain
// it cannot reach is a warning, not a failed logout, and the record is kept
// so the next attempt still reaches the credential.
func TestDeleteLoginTokenWarnsOnUnreachableKeychainWithALingeringRecord(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod"}, "in-file"))
	storeIdentity(t, dir, "prod", "in-keychain")
	keyring.MockInitWithError(errors.New("dbus: no session bus"))
	t.Cleanup(keyring.MockInit)

	from, notices, err := DeleteLoginToken(t.Context(), dir, ServerRef{Name: "prod", URL: "https://prod.ex"}, "file")
	require.NoError(t, err)
	assert.Equal(t, []string{"file"}, from)
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0], "keychain unreachable")

	rec, rerr := readIdentityRecord(dir, "prod")
	require.NoError(t, rerr)
	assert.NotNil(t, rec, "the record is kept for the next attempt")
}

// TestDeleteLoginTokenFailsOnUnreachableKeychainForAKeychainDirectory
// asserts the other half: a directory that declares the keychain fails its
// logout loudly rather than reporting a partial delete.
func TestDeleteLoginTokenFailsOnUnreachableKeychainForAKeychainDirectory(t *testing.T) {
	dir := t.TempDir()
	storeIdentity(t, dir, "prod", "in-keychain")
	keyring.MockInitWithError(errors.New("dbus: no session bus"))
	t.Cleanup(keyring.MockInit)

	_, _, err := DeleteLoginToken(t.Context(), dir, ServerRef{Name: "prod", URL: "https://prod.ex"}, "keychain")
	require.ErrorContains(t, err, "no session bus")

	rec, rerr := readIdentityRecord(dir, "prod")
	require.NoError(t, rerr)
	assert.NotNil(t, rec)
}

// TestDeleteLoginTokenUnreachableKeychainCarriesThePinHint asserts that
// the logout path fails with the same remedy the read path gives: a
// directory whose keychain was chosen by the pin is told how to undo that
// pin, which is the whole point of the hint.
func TestDeleteLoginTokenUnreachableKeychainCarriesThePinHint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvCredentialStore, "")
	require.NoError(t, WritePin(dir, "keychain"))
	storeIdentity(t, dir, "prod", "in-keychain")
	keyring.MockInitWithError(errors.New("dbus: no session bus"))
	t.Cleanup(keyring.MockInit)

	_, _, err := DeleteLoginToken(t.Context(), dir, ServerRef{Name: "prod", URL: "https://prod.ex"}, "auto")
	require.ErrorContains(t, err, "no session bus")
	require.ErrorContains(t, err, "credential store pinned to keychain")
	require.ErrorContains(t, err, "delete the pin to re-probe")
}

// TestFilePolicyNeverTouchesTheKeychain asserts what a directory that
// declares the file store costs: no keychain call on any of the three
// credential operations, and no identity directory to show for them.
func TestFilePolicyNeverTouchesTheKeychain(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	t.Setenv(EnvCredentialStore, "")
	ref := ServerRef{Name: "prod", URL: "https://prod.ex"}
	store, err := OpenStore("file", dir)
	require.NoError(t, err)

	var ops []string
	SetKeyringObserverForTest(func(op string) { ops = append(ops, op) })
	t.Cleanup(func() { SetKeyringObserverForTest(nil) })

	require.NoError(t, store.SetToken(ref, "in-file"))
	tok, err := store.GetToken(ref)
	require.NoError(t, err)
	assert.Equal(t, "in-file", tok)
	_, _, _, err = DeleteTokenEverywhere(t.Context(), dir, ref, "file")
	require.NoError(t, err)

	assert.Empty(t, ops, "a file-policy directory makes no keychain call")
	assert.NoDirExists(t, identityDirPath(dir))
}

// TestDeleteLoginTokenSkipsAReplacedCredential asserts the delete verdict
// for a slot another config directory sharing this identity now owns: the
// keychain is left alone, the local record goes, and the caller is told.
func TestDeleteLoginTokenSkipsAReplacedCredential(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	account := storeIdentity(t, dir, "prod", "ours")
	require.NoError(t, keyring.Set(keyringService, account, "theirs"))

	from, notices, err := DeleteLoginToken(t.Context(), dir, ServerRef{Name: "prod", URL: "https://prod.ex"}, "keychain")
	require.NoError(t, err)
	assert.NotContains(t, from, "keychain")
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0], "another config directory sharing this identity")

	tok, gerr := keyring.Get(keyringService, account)
	require.NoError(t, gerr)
	assert.Equal(t, "theirs", tok, "the other directory keeps its credential")

	rec, rerr := readIdentityRecord(dir, "prod")
	require.NoError(t, rerr)
	assert.Nil(t, rec, "this directory's record goes either way")
}

func TestDeleteTokenEverywhereAlsoDeletesExecCache(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	store := NewFileStore(dir)
	require.NoError(t, store.SetToken(ServerRef{Name: "prod"}, "in-file"))
	require.NoError(t, store.SetCachedCred(CacheKey("kubeuser", "prod.ex", "c-1"), "cached-tok", time.Time{}, "kubeuser", "prod.ex", "c-1"))

	_, deletedCache, _, err := DeleteTokenEverywhere(t.Context(), dir, ServerRef{Name: "prod", URL: "https://prod.ex"}, "file")
	require.NoError(t, err)
	assert.Equal(t, []string{CacheKey("kubeuser", "prod.ex", "c-1")}, deletedCache)

	_, _, gerr := store.GetCachedCred(CacheKey("kubeuser", "prod.ex", "c-1"))
	assert.ErrorIs(t, gerr, ErrNoToken, "logout's single delete point must also delete the exec-credential cache")
}

// TestDeleteTokenEverywhereSurfacesExecCacheLoosePermissionNotice asserts
// that DeleteTokenEverywhere's exec-credential-cache delete drains its
// throwaway *fileStore's own notice buffer: a loose-permission notice on
// token-cache.yaml must reach the function's returned notices, not go out
// of scope with the throwaway store. TestDeleteTokenEverywhereAlsoDeletesExecCache
// never chmods the cache file loose, so it would not catch a regression here.
func TestDeleteTokenEverywhereSurfacesExecCacheLoosePermissionNotice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("loose-permission notice is intentionally disabled on Windows; ACL enforcement is the guarantee there")
	}
	keyring.MockInit()
	dir := t.TempDir()
	store := NewFileStore(dir)
	require.NoError(t, store.SetToken(ServerRef{Name: "prod"}, "in-file"))
	require.NoError(t, store.SetCachedCred(CacheKey("kubeuser", "prod.ex", "c-1"), "cached-tok", time.Time{}, "kubeuser", "prod.ex", "c-1"))
	require.NoError(t, os.Chmod(filepath.Join(dir, "token-cache.yaml"), 0o644))

	_, deletedCache, notices, err := DeleteTokenEverywhere(t.Context(), dir, ServerRef{Name: "prod", URL: "https://prod.ex"}, "file")
	require.NoError(t, err)
	assert.Equal(t, []string{CacheKey("kubeuser", "prod.ex", "c-1")}, deletedCache)
	require.Len(t, notices, 1, "the throwaway exec-cache store's loose-permission notice must reach the caller")
	assert.Contains(t, notices[0], "group/world-readable")
}

// TestDeleteAllExecCacheSurfacesLoosePermissionNotice asserts the same fix for
// DeleteAllExecCache (logout --all's single-wipe path): its own throwaway
// *fileStore's notice buffer must be drained and returned, not dropped.
func TestDeleteAllExecCacheSurfacesLoosePermissionNotice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("loose-permission notice is intentionally disabled on Windows; ACL enforcement is the guarantee there")
	}
	dir := t.TempDir()
	store := NewFileStore(dir)
	require.NoError(t, store.SetCachedCred(CacheKey("kubeuser", "prod.ex", "c-1"), "cached-tok", time.Time{}, "kubeuser", "prod.ex", "c-1"))
	require.NoError(t, os.Chmod(filepath.Join(dir, "token-cache.yaml"), 0o644))

	keys, notices, err := DeleteAllExecCache(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{CacheKey("kubeuser", "prod.ex", "c-1")}, keys)
	require.Len(t, notices, 1, "the throwaway store's loose-permission notice must reach the caller")
	assert.Contains(t, notices[0], "group/world-readable")
}

func TestDeleteTokenEverywhereFailsOnUnreadableFileStore(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	// corrupt credentials.yaml: GetToken returns a non-ErrNoToken error
	if err := os.WriteFile(filepath.Join(dir, "credentials.yaml"), []byte("\t: not yaml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := DeleteTokenEverywhere(t.Context(), dir, ServerRef{Name: "prod", URL: "https://prod.ex"}, "file")
	if err == nil {
		t.Fatal("DeleteTokenEverywhere must fail, not report success, when the file store is unreadable (a live token may remain)")
	}
}

func TestDeleteExecCacheForServerRemovesMatchingHostOnly(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	require.NoError(t, store.SetCachedCred(CacheKey("kubeuser", "acme.ex", "c-1"), "tok-acme", time.Time{}, "kubeuser", "acme.ex", "c-1"))
	require.NoError(t, store.SetCachedCred(CacheKey("kubeuser", "other.ex", "c-1"), "tok-other", time.Time{}, "kubeuser", "other.ex", "c-1"))
	require.NoError(t, store.SetCachedCred("legacy-bare", "tok-legacy", time.Time{}, "", "", ""))

	deleted, err := DeleteExecCacheForServer(store, "https://acme.ex")
	require.NoError(t, err)
	assert.Equal(t, []string{CacheKey("kubeuser", "acme.ex", "c-1")}, deleted)

	_, _, err = store.GetCachedCred(CacheKey("kubeuser", "acme.ex", "c-1"))
	require.ErrorIs(t, err, ErrNoToken, "matching entry must be deleted")
	_, _, err = store.GetCachedCred(CacheKey("kubeuser", "other.ex", "c-1"))
	require.NoError(t, err, "non-matching entry must be kept")
	_, _, err = store.GetCachedCred("legacy-bare")
	assert.NoError(t, err, "legacy bare entry (no Server) must be kept")
}

// Round-2 pin: two config entries with different --name values ("acme-admin",
// "acme-viewer") that both point at the same host each cache their own
// exec-credential entry. Logging out one deletes BOTH - deliberately, not a
// bug: matching by config-entry name is structurally unreliable (a cache
// entry embeds the name only by kubeconfig naming convention), so
// host-wide delete is the safe direction. See DeleteTokenEverywhere's doc comment.
func TestDeleteExecCacheForServerDeletesAllNamesSharingOneHost(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	require.NoError(t, store.SetCachedCred(CacheKey("acme-admin", "acme.ex", "c-1"), "tok-admin", time.Time{}, "acme-admin", "acme.ex", "c-1"))
	require.NoError(t, store.SetCachedCred(CacheKey("acme-viewer", "acme.ex", "c-1"), "tok-viewer", time.Time{}, "acme-viewer", "acme.ex", "c-1"))

	deleted, err := DeleteExecCacheForServer(store, "https://acme.ex")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		CacheKey("acme-admin", "acme.ex", "c-1"),
		CacheKey("acme-viewer", "acme.ex", "c-1"),
	}, deleted, "both names sharing one host must be deleted together")

	_, _, err = store.GetCachedCred(CacheKey("acme-admin", "acme.ex", "c-1"))
	require.ErrorIs(t, err, ErrNoToken)
	_, _, err = store.GetCachedCred(CacheKey("acme-viewer", "acme.ex", "c-1"))
	assert.ErrorIs(t, err, ErrNoToken)
}

func TestDeleteExecCacheForServerEmptyHostIsNoop(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	require.NoError(t, store.SetCachedCred("legacy-bare", "tok", time.Time{}, "", "", ""))

	deleted, err := DeleteExecCacheForServer(store, "")
	require.NoError(t, err)
	assert.Nil(t, deleted)

	_, _, err = store.GetCachedCred("legacy-bare")
	assert.NoError(t, err, "empty host must not touch the cache")
}

// TestKeyringDeleteTokenLeavesTheFileStore asserts the keychain store
// deletes from the keychain only. The two stores hold separate
// credentials, and on a directory whose store is changing, deleting the
// file entry here would remove the credential a login just committed.
func TestKeyringDeleteTokenLeavesTheFileStore(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod"}, "in-file"))
	s := newPinnedKeyringStore(dir, false)

	require.NoError(t, s.DeleteToken(ServerRef{Name: "prod"}))

	tok, err := NewFileStore(dir).GetToken(ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "in-file", tok)
}

func TestListCachedCredsWriteBackFailureIsBestEffort(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewFileStore(dir)
	require.NoError(t, s.SetCachedCred("live", "tok1", time.Now().Add(time.Hour), "", "", ""))
	require.NoError(t, s.SetCachedCred("expired", "tok2", time.Now().Add(-time.Minute), "", "", ""))

	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	list, err := s.ListCachedCreds()
	require.NoError(t, err, "list must succeed even when prune write-back fails")
	require.Len(t, list, 1)
	assert.Equal(t, "live", list[0].Name, "live entry must be returned")
}

// TestKeyringUnreachableGetFailsWithThePinHint asserts that a directory
// pinned to the keychain fails its read when the keychain cannot be
// reached, with the way out in the message. It does not fall back to the
// file store: a token there is not this directory's keychain credential,
// and a headless machine uses a directory that declares the file store.
func TestKeyringUnreachableGetFailsWithThePinHint(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod"}, "file-token"))
	storeIdentity(t, dir, "prod", "in-keychain")
	keyring.MockInitWithError(errors.New("dbus: no session bus"))
	t.Cleanup(keyring.MockInit)

	s := newPinnedKeyringStore(dir, true)
	_, err := s.GetToken(ServerRef{Name: "prod"})
	require.ErrorContains(t, err, "no session bus")
	require.ErrorContains(t, err, "credential store pinned to keychain")
	assert.NotErrorIs(t, err, ErrNoToken, "the credential may be in the keychain this read could not reach")
}

// TestKeyringUnreachableDeleteFailsWithThePinHint asserts the same for a
// delete: the record is kept, and the file store is not touched as a
// consolation.
func TestKeyringUnreachableDeleteFailsWithThePinHint(t *testing.T) {
	dir := t.TempDir()
	fs := NewFileStore(dir)
	require.NoError(t, fs.SetToken(ServerRef{Name: "prod"}, "file-token"))
	storeIdentity(t, dir, "prod", "in-keychain")
	keyring.MockInitWithError(errors.New("dbus: no session bus"))
	t.Cleanup(keyring.MockInit)

	s := newPinnedKeyringStore(dir, true)
	err := s.DeleteToken(ServerRef{Name: "prod"})
	require.ErrorContains(t, err, "no session bus")
	require.ErrorContains(t, err, "credential store pinned to keychain")

	rec, rerr := readIdentityRecord(dir, "prod")
	require.NoError(t, rerr)
	assert.NotNil(t, rec, "the record is kept while the credential may still be there")

	tok, gerr := fs.GetToken(ServerRef{Name: "prod"})
	require.NoError(t, gerr)
	assert.Equal(t, "file-token", tok)
}

// TestKeyringSetDataTooBigIsNotMisdiagnosedAsUnreachable: an oversized
// token must surface go-keyring's own message plainly, not the
// "unreachable... locked keychain? no D-Bus session?" remedy - a
// data-size failure is not a connectivity one (XP-6 fix round, finding 6).
func TestKeyringSetDataTooBigIsNotMisdiagnosedAsUnreachable(t *testing.T) {
	dir := t.TempDir()
	keyring.MockInitWithError(keyring.ErrSetDataTooBig)
	t.Cleanup(keyring.MockInit)

	s := newPinnedKeyringStore(dir, true)
	err := s.SetToken(ServerRef{Name: "prod"}, "oversized-token")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "unreachable")
	assert.ErrorContains(t, err, keyring.ErrSetDataTooBig.Error())
}

// TestKeyringUnreachableSetRefusesWithRemedy: a login write under an
// unreachable keychain must hard-fail with the env-var remedy and must
// NOT create a plaintext credentials file - the anti-silent-downgrade
// guarantee (XP-6, decided: guarantee wins on the write path).
func TestKeyringUnreachableSetRefusesWithRemedy(t *testing.T) {
	dir := t.TempDir()
	keyring.MockInitWithError(errors.New("dbus: no session bus"))
	t.Cleanup(keyring.MockInit)

	s := newPinnedKeyringStore(dir, true)
	err := s.SetToken(ServerRef{Name: "prod"}, "new-token")
	require.Error(t, err)
	require.ErrorContains(t, err, "unreachable")
	require.ErrorContains(t, err, EnvCredentialStore+"=file")
	_, statErr := os.Stat(filepath.Join(dir, "credentials.yaml"))
	assert.True(t, os.IsNotExist(statErr), "plaintext credentials.yaml must not be created")
}

func TestCheckPolicyExec(t *testing.T) {
	t.Parallel()
	// valid
	_, err := OpenStore("exec:vault", t.TempDir())
	require.NoError(t, err)
	// invalid names are hard config errors
	for _, p := range []string{"exec:", "exec:Va ult", "exec:va/ult", `exec:va\ult`, "exec:-x"} {
		_, err := OpenStore(p, t.TempDir())
		assert.Error(t, err, "policy %q", p)
	}
}

func TestCanonicalServerIdentityHTTPSEquivalentSpellingsShareOneIdentity(t *testing.T) {
	t.Parallel()
	// Equivalent HTTPS spellings must produce the same full canonical identity.
	spellings := []string{
		"https://host",
		"https://host:443",
		"HTTPS://HOST:443",
		"HTTPS://host",
	}
	want := CanonicalServerIdentity("https://host")
	require.NotEmpty(t, want)
	for _, s := range spellings {
		assert.Equal(t, want, CanonicalServerIdentity(s), "spelling %q must canonicalize to %q", s, want)
	}
}

func TestCanonicalServerIdentityHTTPEquivalentSpellingsShareOneIdentity(t *testing.T) {
	t.Parallel()
	spellings := []string{
		"http://host",
		"http://host:80",
		"HTTP://HOST:80",
		"HTTP://host",
	}
	want := CanonicalServerIdentity("http://host")
	require.NotEmpty(t, want)
	for _, s := range spellings {
		assert.Equal(t, want, CanonicalServerIdentity(s), "spelling %q must canonicalize to %q", s, want)
	}
}

func TestCanonicalServerIdentityHTTPAndHTTPSAreDistinct(t *testing.T) {
	t.Parallel()
	// The P1 finding: same host, different scheme - must produce DIFFERENT identities.
	assert.NotEqual(t, CanonicalServerIdentity("http://host"), CanonicalServerIdentity("https://host"),
		"http and https identities must be distinct")
	assert.NotEqual(t, CanonicalServerIdentity("http://host:80"), CanonicalServerIdentity("https://host:443"),
		"equivalent http and https spellings must still differ in identity")
}

func TestCanonicalServerHostEquivalentHTTPSSpellingsShareOneKey(t *testing.T) {
	t.Parallel()
	// Equivalent HTTPS spellings must produce identical cache-key components so
	// a credential cached via one spelling is retrievable via the other.
	spellings := []string{
		"https://host",
		"https://host:443",
		"HTTPS://HOST:443",
		"HTTPS://host",
	}
	want := canonicalServerHost("https://host")
	require.NotEmpty(t, want)
	for _, s := range spellings {
		assert.Equal(t, want, canonicalServerHost(s), "spelling %q must canonicalize to %q", s, want)
	}
}

func TestCanonicalServerHostHTTPEquivalentSpellings(t *testing.T) {
	t.Parallel()
	spellings := []string{
		"http://host",
		"http://host:80",
		"HTTP://HOST:80",
		"HTTP://host",
	}
	want := canonicalServerHost("http://host")
	require.NotEmpty(t, want)
	for _, s := range spellings {
		assert.Equal(t, want, canonicalServerHost(s), "spelling %q must canonicalize to %q", s, want)
	}
}

// TestFileStoreCacheLockPreventsLostUpdate is the cross-instance lost-update
// regression test for the token-cache.yaml flock. It simulates two kubectl
// processes (two separate fileStore instances sharing the same dir - each has
// its own mu, so they do NOT contend on mu) writing to the cache concurrently.
// setCachedLock fires while G1 holds the flock (after its read, before its
// write). G2 tries to acquire the flock and blocks. When G1 releases, G2 reads
// the updated file and writes its own entry - both keys must survive.
// No t.Parallel(): uses the package-level setCachedLock hook.
func TestFileStoreCacheLockPreventsLostUpdate(t *testing.T) {
	dir := t.TempDir()
	store1 := NewFileStore(dir)
	store2 := NewFileStore(dir)

	inRead := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var signalOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }

	setCachedLock = func() {
		signalOnce.Do(func() { close(inRead) }) // signal on first caller (G1) only
		<-release                               // both callers hold until test unblocks
	}
	t.Cleanup(func() {
		setCachedLock = nil
		unblock() // ensure we never leak the goroutines on test failure
	})

	err1 := make(chan error, 1)
	go func() {
		err1 <- store1.SetCachedCred("key-a", "tok-a", time.Now().Add(time.Hour), "a", "r.ex", "")
	}()

	select {
	case <-inRead:
	case e := <-err1:
		t.Fatalf("store1 finished before reaching the lock hook: %v", e)
	case <-time.After(10 * time.Second):
		t.Fatal("store1 never reached the lock hook")
	}

	// G1 is now holding the flock (inside the hook). Start G2 - it must block
	// on the flock until G1 releases.
	err2 := make(chan error, 1)
	go func() {
		err2 <- store2.SetCachedCred("key-b", "tok-b", time.Now().Add(time.Hour), "b", "r.ex", "")
	}()

	// Unblock G1: it writes key-a and releases the flock.
	// G2 then acquires the flock, reads the file (which now has key-a), and writes key-b.
	unblock()

	require.NoError(t, <-err1, "store1.SetCachedCred must succeed")
	require.NoError(t, <-err2, "store2.SetCachedCred must succeed")

	// Both keys must survive - no lost update.
	_, _, errA := store1.GetCachedCred("key-a")
	require.NoError(t, errA, "key-a must survive after concurrent write")
	_, _, errB := store1.GetCachedCred("key-b")
	assert.NoError(t, errB, "key-b must survive after concurrent write")
}

// TestFileStoreCacheLockPreventsLostUpdateOnExpiry asserts the reviewer's exact
// interleaving: SetCachedCred (A, adding key-a) races GetCachedCred (B,
// pruning an expired key-b). Without the fix, B reads the cache before A's
// write, then prunes and writes a snapshot that does not include key-a - a
// lost update. With the fix, B re-reads under the flock after A releases it
// and therefore includes key-a in its written snapshot.
// No t.Parallel(): uses the package-level setCachedLock and getCachedPruneHook.
func TestFileStoreCacheLockPreventsLostUpdateOnExpiry(t *testing.T) {
	dir := t.TempDir()
	store1 := NewFileStore(dir)
	store2 := NewFileStore(dir)

	// Pre-seed: key-b already expired, key-c valid.
	require.NoError(t, store1.SetCachedCred("key-b", "tok-b", time.Now().Add(-time.Hour), "b", "r.ex", ""))
	require.NoError(t, store1.SetCachedCred("key-c", "tok-c", time.Now().Add(time.Hour), "c", "r.ex", ""))

	inRead := make(chan struct{})
	release := make(chan struct{})
	bReady := make(chan struct{})
	var aOnce, bOnce sync.Once

	setCachedLock = func() {
		aOnce.Do(func() { close(inRead) }) // A signals it holds the flock
		<-release                          // A waits until test unblocks it
	}
	getCachedPruneHook = func() {
		bOnce.Do(func() { close(bReady) }) // B signals it is about to acquire the flock
	}
	t.Cleanup(func() {
		setCachedLock = nil
		getCachedPruneHook = nil
		// Ensure goroutines are never leaked on test failure.
		select {
		case <-release:
		default:
			close(release)
		}
	})

	errA := make(chan error, 1)
	go func() {
		errA <- store1.SetCachedCred("key-a", "tok-a", time.Now().Add(time.Hour), "a", "r.ex", "")
	}()

	select {
	case <-inRead:
	case e := <-errA:
		t.Fatalf("store1 finished before the lock hook: %v", e)
	case <-time.After(10 * time.Second):
		t.Fatal("store1 never reached the lock hook")
	}

	// A holds the flock. Start B (GetCachedCred on the expired key-b).
	errB := make(chan error, 1)
	go func() {
		_, _, err := store2.GetCachedCred("key-b")
		if errors.Is(err, ErrNoToken) {
			err = nil // expected: the expired entry is pruned
		}
		errB <- err
	}()

	// Wait for B to complete its lock-free read and reach the prune hook,
	// confirming it is about to call withCacheLock (and will block on A's flock).
	select {
	case <-bReady:
	case <-time.After(10 * time.Second):
		t.Fatal("store2 GetCachedCred never reached the prune hook")
	}

	// Unblock A: it writes {key-b(expired), key-c, key-a} and releases the flock.
	// B then acquires the flock, re-reads that snapshot, prunes key-b, and
	// writes {key-c, key-a} - both effects survive.
	close(release)

	require.NoError(t, <-errA, "store1.SetCachedCred must succeed")
	require.NoError(t, <-errB, "store2.GetCachedCred (expired entry) must succeed")

	// A's effect: key-a must be present.
	_, _, errGet := store1.GetCachedCred("key-a")
	require.NoError(t, errGet, "key-a (SetCachedCred effect) must survive concurrent GetCachedCred prune")

	// B's effect: key-b must be pruned.
	_, _, errGet = store1.GetCachedCred("key-b")
	require.ErrorIs(t, errGet, ErrNoToken, "expired key-b must be pruned by GetCachedCred")

	// Unrelated key-c must be unaffected.
	_, _, errGet = store1.GetCachedCred("key-c")
	assert.NoError(t, errGet, "key-c must survive unaffected")
}

// TestFileStoreCacheGetServesConcurrentlyRefreshedKey asserts the same-key
// interleaving of the prune race: GetCachedCred finds the key expired in its
// lock-free read; before it acquires the flock, a concurrent SetCachedCred
// (a re-auth for the same server+cluster+user in another process) refreshes
// the SAME key with a valid credential. The lock-held re-read must detect the
// refresh, keep the entry, and serve the fresh credential - not delete the
// just-created token and return ErrNoToken.
// No t.Parallel(): uses the package-level getCachedPruneHook.
func TestFileStoreCacheGetServesConcurrentlyRefreshedKey(t *testing.T) {
	dir := t.TempDir()
	store1 := NewFileStore(dir)
	store2 := NewFileStore(dir)

	expiry := time.Now().Add(time.Hour)
	require.NoError(t, store1.SetCachedCred("key-a", "tok-stale", time.Now().Add(-time.Hour), "a", "r.ex", ""))

	// The hook fires after the lock-free read (which saw key-a expired) and
	// before the flock is acquired: refresh key-a synchronously from here.
	getCachedPruneHook = func() {
		getCachedPruneHook = nil // fire once
		require.NoError(t, store2.SetCachedCred("key-a", "tok-fresh", expiry, "a", "r.ex", ""))
	}
	t.Cleanup(func() { getCachedPruneHook = nil })

	tok, gotExpiry, err := store1.GetCachedCred("key-a")
	require.NoError(t, err, "a concurrently refreshed key must be served, not pruned")
	assert.Equal(t, "tok-fresh", tok)
	assert.WithinDuration(t, expiry, gotExpiry, time.Second)

	// The refreshed entry must survive on disk for the next invocation.
	tok, _, err = store1.GetCachedCred("key-a")
	require.NoError(t, err, "the refreshed entry must not have been deleted")
	assert.Equal(t, "tok-fresh", tok)
}

// TestDeleteAllCachedCredsHonorsCacheLock asserts the round-8 wipe race:
// DeleteAllCachedCreds must block on the flock while a concurrent
// SetCachedCred's read-modify-write is in flight. Without the lock, the
// Remove lands mid-write and the writer's write-back silently resurrects
// the "deleted" entries - no error surfaced to either process.
// No t.Parallel(): uses the package-level setCachedLock hook.
func TestDeleteAllCachedCredsHonorsCacheLock(t *testing.T) {
	dir := t.TempDir()
	store1 := NewFileStore(dir)
	store2 := NewFileStore(dir)

	require.NoError(t, store1.SetCachedCred("key-a", "tok-a", time.Now().Add(time.Hour), "a", "r.ex", ""))

	inWrite := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	setCachedLock = func() {
		once.Do(func() { close(inWrite) }) // the concurrent writer holds the flock
		<-release
	}
	t.Cleanup(func() {
		setCachedLock = nil
		select {
		case <-release:
		default:
			close(release)
		}
	})

	errSet := make(chan error, 1)
	go func() {
		errSet <- store1.SetCachedCred("key-b", "tok-b", time.Now().Add(time.Hour), "b", "r.ex", "")
	}()
	select {
	case <-inWrite:
	case e := <-errSet:
		t.Fatalf("SetCachedCred finished before the lock hook: %v", e)
	case <-time.After(10 * time.Second):
		t.Fatal("SetCachedCred never reached the lock hook")
	}

	// The writer holds the flock (mid read-modify-write). Start the wipe -
	// it must wait for the write-back, then remove everything, so neither
	// the pre-seeded key-a nor the concurrently written key-b survives.
	// The hook is set only now, after the writer's own flock wait already
	// completed above, so it fires for the wipe's wait alone.
	wipeWaiting := make(chan struct{})
	restoreFlockHook := SetWaitForFlockHookForTest(func() { close(wipeWaiting) })
	defer restoreFlockHook()
	errDel := make(chan error, 1)
	go func() { errDel <- store2.DeleteAllCachedCreds() }()

	// Wait until the wipe reaches the flock before releasing the writer, so
	// the assertions below exercise the queued-behind-writer order
	// deterministically instead of by chance.
	<-wipeWaiting
	close(release)

	require.NoError(t, <-errSet, "SetCachedCred must succeed")
	require.NoError(t, <-errDel, "DeleteAllCachedCreds must succeed")

	_, _, err := store1.GetCachedCred("key-a")
	require.ErrorIs(t, err, ErrNoToken, "key-a must stay wiped, not be resurrected by the concurrent write-back")
	_, _, err = store1.GetCachedCred("key-b")
	assert.ErrorIs(t, err, ErrNoToken, "key-b (written before the wipe acquired the lock) must be wiped too")
}

// TestSetTokenLostUpdateWithoutConfigLock asserts the stale-snapshot bug class
// for credentials.yaml: two fileStore instances (each with its own mu,
// modelling two separate processes) both read the file, both update their
// in-memory map, and both write - the second write overwrites the first.
// The test PASSES by asserting that loss; it documents the bug class that
// wrapping SetToken calls in WithLock closes.
// No t.Parallel(): uses the package-level setTokenHook.
func TestSetTokenLostUpdateWithoutConfigLock(t *testing.T) {
	dir := t.TempDir()
	store1 := NewFileStore(dir).(*fileStore)
	store2 := NewFileStore(dir).(*fileStore)

	// Buffer size 2 so both goroutines can signal without blocking on each
	// other. Each sends once, then blocks on release.
	hookCalls := make(chan struct{}, 2)
	release := make(chan struct{})

	setTokenHook = func() {
		hookCalls <- struct{}{}
		<-release
	}
	t.Cleanup(func() {
		setTokenHook = nil
		select {
		case <-release:
		default:
			close(release)
		}
	})

	err1 := make(chan error, 1)
	go func() { err1 <- store1.SetToken(ServerRef{Name: "alpha"}, "token-alpha") }()

	err2 := make(chan error, 1)
	go func() { err2 <- store2.SetToken(ServerRef{Name: "beta"}, "token-beta") }()

	// Wait for both goroutines to reach the hook (both have read, neither
	// has written). They now hold stale in-memory snapshots of the same
	// empty file.
	for i := range 2 {
		select {
		case <-hookCalls:
		case <-time.After(10 * time.Second):
			t.Fatalf("goroutine %d never reached setTokenHook", i+1)
		}
	}

	// Release both simultaneously. Each writes a 1-entry map; the second
	// write atomically overwrites the first.
	close(release)

	require.NoError(t, <-err1)
	require.NoError(t, <-err2)

	// Exactly one token survives. The assertion documents the bug class:
	// without an outer lock, concurrent SetToken calls on separate store
	// instances produce a deterministic lost update.
	alpha, errA := store1.GetToken(ServerRef{Name: "alpha"})
	beta, errB := store1.GetToken(ServerRef{Name: "beta"})
	bothPresent := (errA == nil && errB == nil)
	assert.False(t, bothPresent,
		"both tokens must NOT survive: without WithLock, one write clobbers the other (alpha=%q errA=%v, beta=%q errB=%v)",
		alpha, errA, beta, errB)
}

// TestWithLockSerializesSetToken proves that wrapping SetToken calls in
// WithLock eliminates the lost update: the second caller blocks on the
// process mutex until the first completes its write, so the second's read
// sees the already-written state and both tokens survive.
// No t.Parallel(): uses the package-level setTokenHook.
func TestWithLockSerializesSetToken(t *testing.T) {
	dir := t.TempDir()
	store1 := NewFileStore(dir).(*fileStore)
	store2 := NewFileStore(dir).(*fileStore)

	g1InHook := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	setTokenHook = func() {
		once.Do(func() { close(g1InHook) }) // fires on the first caller only
		<-release
	}
	t.Cleanup(func() {
		setTokenHook = nil
		select {
		case <-release:
		default:
			close(release)
		}
	})

	err1 := make(chan error, 1)
	go func() {
		err1 <- WithLock(t.Context(), dir, func() error {
			return store1.SetToken(ServerRef{Name: "alpha"}, "token-alpha")
		})
	}()

	// Wait for G1 to reach setTokenHook - it holds WithLock's process
	// mutex and has read credentials.yaml but not yet written.
	select {
	case <-g1InHook:
	case <-time.After(10 * time.Second):
		t.Fatal("G1 never reached setTokenHook")
	}

	// G2 attempts WithLock; it must block on the process mutex since G1
	// still holds it via the hook. The semaphore wait hook fires as G2
	// reaches the mutex, so the test waits for that fact instead of a
	// window of real time.
	g2AtSem := make(chan struct{})
	var atSemOnce sync.Once
	restoreSem := SetSemWaitHookForTest(func() { atSemOnce.Do(func() { close(g2AtSem) }) })
	t.Cleanup(restoreSem)
	err2 := make(chan error, 1)
	go func() {
		err2 <- WithLock(t.Context(), dir, func() error {
			return store2.SetToken(ServerRef{Name: "beta"}, "token-beta")
		})
	}()
	select {
	case <-g2AtSem:
	case <-time.After(10 * time.Second):
		t.Fatal("G2 never reached the process mutex")
	}
	restoreSem()
	// G2 is at the mutex and G1 still holds it: G2 cannot have completed.
	select {
	case e := <-err2:
		t.Fatalf("G2 completed before G1 released WithLock: %v", e)
	default:
	}

	// Release G1. It writes token-alpha and exits WithLock. G2 then
	// acquires the lock, reads the updated file (which now has alpha), and
	// writes beta alongside it.
	close(release)

	require.NoError(t, <-err1, "store1 SetToken must succeed")
	require.NoError(t, <-err2, "store2 SetToken must succeed")

	// Both tokens must survive - no lost update.
	tok, err := store1.GetToken(ServerRef{Name: "alpha"})
	require.NoError(t, err)
	assert.Equal(t, "token-alpha", tok)
	tok, err = store1.GetToken(ServerRef{Name: "beta"})
	require.NoError(t, err)
	assert.Equal(t, "token-beta", tok)
}

// TestMarkCachedCredReadyIsConditional asserts the readiness write: a
// fingerprint lands only on a record that still holds the same token, is
// idempotent, and leaves a replaced or deleted record alone; the detail
// read returns it with the token.
func TestMarkCachedCredReadyIsConditional(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewFileStore(dir)
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	require.NoError(t, s.SetCachedCred("k", "tok-1", exp, "n", "https://r", "c"))

	marked, err := s.MarkCachedCredReady("k", "tok-1", "fp-a")
	require.NoError(t, err)
	assert.True(t, marked)
	marked, err = s.MarkCachedCredReady("k", "tok-1", "fp-a")
	require.NoError(t, err)
	assert.True(t, marked, "idempotent")
	d, err := s.GetCachedCredDetail("k")
	require.NoError(t, err)
	assert.Equal(t, "tok-1", d.Token)
	assert.Equal(t, []string{"fp-a"}, d.ReadyEndpoints)

	// Another process refreshed the slot: the stale token's mark is refused
	// and the fresh record keeps its own readiness.
	require.NoError(t, s.SetCachedCred("k", "tok-2", exp, "n", "https://r", "c"))
	marked, err = s.MarkCachedCredReady("k", "tok-1", "fp-b")
	require.NoError(t, err)
	assert.False(t, marked)
	d, err = s.GetCachedCredDetail("k")
	require.NoError(t, err)
	assert.Equal(t, "tok-2", d.Token)
	assert.Empty(t, d.ReadyEndpoints, "a replacing write starts with no readiness")

	// Deleted: nothing is resurrected.
	require.NoError(t, s.DeleteCachedCred("k"))
	marked, err = s.MarkCachedCredReady("k", "tok-2", "fp-a")
	require.NoError(t, err)
	assert.False(t, marked)
	_, err = s.GetCachedCredDetail("k")
	require.ErrorIs(t, err, ErrNoToken)

	// Expired: treated as absent.
	require.NoError(t, s.SetCachedCred("k", "tok-3", time.Now().Add(-time.Minute), "n", "https://r", "c"))
	marked, err = s.MarkCachedCredReady("k", "tok-3", "fp-a")
	require.NoError(t, err)
	assert.False(t, marked)
}
