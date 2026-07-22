package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
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
	if err == nil || !strings.Contains(err.Error(), "keychain, file, auto") {
		t.Fatalf("unknown policy: got %v, want error listing valid values", err)
	}
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
	// re-probes — this is the pin's own once-per-dir contract. probes still
	// pins the underlying keyringUsableOnce process-wide memoization: a
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
	assert.ErrorIs(t, err, ErrNoToken)
	_, _, err = s.GetCachedCred("acme/u/c2")
	assert.ErrorIs(t, err, ErrNoToken)
}

func TestCachedCredExpiredPurgeErrorIsPropagated(t *testing.T) {
	dir := t.TempDir()
	s := NewFileStore(dir).(*fileStore)
	require.NoError(t, s.SetCachedCred("acme/u/c", "tok", time.Now().Add(-time.Minute), "", "", ""))

	tok, _, err := s.GetCachedCred("acme/u/c")
	assert.ErrorIs(t, err, ErrNoToken)
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

	orig := logrus.StandardLogger().Out
	var buf bytes.Buffer
	logrus.SetOutput(&buf)
	defer logrus.SetOutput(orig)

	// Loose (0644): warns once, then deduped for the same path.
	looseDir := t.TempDir()
	loose := NewFileStore(looseDir)
	require.NoError(t, loose.SetToken(ServerRef{Name: "acme"}, "id:sec"))
	require.NoError(t, os.Chmod(filepath.Join(looseDir, "credentials.yaml"), 0o644))
	_, err := loose.GetToken(ServerRef{Name: "acme"})
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "group/world-readable", "0644 credentials must warn")

	buf.Reset()
	_, err = loose.GetToken(ServerRef{Name: "acme"})
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "group/world-readable", "warning must fire once per path per process")

	// Tight (0600), fresh path: silent.
	buf.Reset()
	tight := NewFileStore(t.TempDir())
	require.NoError(t, tight.SetToken(ServerRef{Name: "acme"}, "id:sec")) // written 0600
	_, err = tight.GetToken(ServerRef{Name: "acme"})
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "group/world-readable", "0600 credentials must be silent")
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

func TestKeyringGetTokenReadsThroughToFile(t *testing.T) {
	keyring.MockInit() // in-memory keyring, empty
	dir := t.TempDir()
	if err := NewFileStore(dir).SetToken(ServerRef{Name: "prod"}, "id:secret"); err != nil {
		t.Fatal(err)
	}
	s := newPinnedKeyringStore(dir, false)
	tok, err := s.GetToken(ServerRef{Name: "prod"})
	if err != nil || tok != "id:secret" {
		t.Fatalf("read-through: got %q, %v", tok, err)
	}
}

func TestDrainTokenRemovesFromBothBackends(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	_ = keyring.Set(keyringService, "prod", "in-keychain")
	_ = NewFileStore(dir).SetToken(ServerRef{Name: "prod"}, "in-file")
	from, _, err := DrainToken(dir, ServerRef{Name: "prod", URL: "https://prod.ex"}, "file")
	if err != nil {
		t.Fatal(err)
	}
	if len(from) != 2 {
		t.Errorf("deletedFrom = %v, want both backends", from)
	}
	if _, err := keyring.Get(keyringService, "prod"); err == nil {
		t.Error("token still in keychain")
	}
	if _, err := NewFileStore(dir).GetToken(ServerRef{Name: "prod"}); err == nil {
		t.Error("token still in file store")
	}
}

func TestDrainTokenAlsoDrainsExecCache(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	store := NewFileStore(dir)
	require.NoError(t, store.SetToken(ServerRef{Name: "prod"}, "in-file"))
	require.NoError(t, store.SetCachedCred(CacheKey("kubeuser", "prod.ex", "c-1"), "cached-tok", time.Time{}, "kubeuser", "prod.ex", "c-1"))

	_, drainedCache, err := DrainToken(dir, ServerRef{Name: "prod", URL: "https://prod.ex"}, "file")
	require.NoError(t, err)
	assert.Equal(t, []string{CacheKey("kubeuser", "prod.ex", "c-1")}, drainedCache)

	_, _, gerr := store.GetCachedCred(CacheKey("kubeuser", "prod.ex", "c-1"))
	assert.ErrorIs(t, gerr, ErrNoToken, "logout's single drain point must also drain the exec-credential cache")
}

func TestDrainTokenFailsOnUnreadableFileStore(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	// corrupt credentials.yaml: GetToken returns a non-ErrNoToken error
	if err := os.WriteFile(filepath.Join(dir, "credentials.yaml"), []byte("\t: not yaml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := DrainToken(dir, ServerRef{Name: "prod", URL: "https://prod.ex"}, "file")
	if err == nil {
		t.Fatal("DrainToken must fail, not report success, when the file store is unreadable (a live token may remain)")
	}
}

func TestDrainExecCacheForServerRemovesMatchingHostOnly(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	require.NoError(t, store.SetCachedCred(CacheKey("kubeuser", "acme.ex", "c-1"), "tok-acme", time.Time{}, "kubeuser", "acme.ex", "c-1"))
	require.NoError(t, store.SetCachedCred(CacheKey("kubeuser", "other.ex", "c-1"), "tok-other", time.Time{}, "kubeuser", "other.ex", "c-1"))
	require.NoError(t, store.SetCachedCred("legacy-bare", "tok-legacy", time.Time{}, "", "", ""))

	deleted, err := DrainExecCacheForServer(store, "https://acme.ex")
	require.NoError(t, err)
	assert.Equal(t, []string{CacheKey("kubeuser", "acme.ex", "c-1")}, deleted)

	_, _, err = store.GetCachedCred(CacheKey("kubeuser", "acme.ex", "c-1"))
	assert.ErrorIs(t, err, ErrNoToken, "matching entry must be drained")
	_, _, err = store.GetCachedCred(CacheKey("kubeuser", "other.ex", "c-1"))
	assert.NoError(t, err, "non-matching entry must be kept")
	_, _, err = store.GetCachedCred("legacy-bare")
	assert.NoError(t, err, "legacy bare entry (no Server) must be kept")
}

// Round-2 pin: two config entries with different --name values ("acme-admin",
// "acme-viewer") that both point at the same host each cache their own
// exec-credential entry. Logging out one drains BOTH — deliberately, not a
// bug: matching by config-entry name is structurally unreliable (a cache
// entry embeds the name only by kubeconfig naming convention), so
// host-wide drain is the safe direction. See DrainToken's doc comment.
func TestDrainExecCacheForServerDrainsAllNamesSharingOneHost(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	require.NoError(t, store.SetCachedCred(CacheKey("acme-admin", "acme.ex", "c-1"), "tok-admin", time.Time{}, "acme-admin", "acme.ex", "c-1"))
	require.NoError(t, store.SetCachedCred(CacheKey("acme-viewer", "acme.ex", "c-1"), "tok-viewer", time.Time{}, "acme-viewer", "acme.ex", "c-1"))

	deleted, err := DrainExecCacheForServer(store, "https://acme.ex")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		CacheKey("acme-admin", "acme.ex", "c-1"),
		CacheKey("acme-viewer", "acme.ex", "c-1"),
	}, deleted, "both names sharing one host must be drained together")

	_, _, err = store.GetCachedCred(CacheKey("acme-admin", "acme.ex", "c-1"))
	assert.ErrorIs(t, err, ErrNoToken)
	_, _, err = store.GetCachedCred(CacheKey("acme-viewer", "acme.ex", "c-1"))
	assert.ErrorIs(t, err, ErrNoToken)
}

func TestDrainExecCacheForServerEmptyHostIsNoop(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	require.NoError(t, store.SetCachedCred("legacy-bare", "tok", time.Time{}, "", "", ""))

	deleted, err := DrainExecCacheForServer(store, "")
	require.NoError(t, err)
	assert.Nil(t, deleted)

	_, _, err = store.GetCachedCred("legacy-bare")
	assert.NoError(t, err, "empty host must not touch the cache")
}

func TestKeyringDeleteTokenDrainsFileStore(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	// Token lives only in the file store (e.g. written under an earlier file pin).
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod"}, "in-file"))
	s := newPinnedKeyringStore(dir, false)
	require.NoError(t, s.DeleteToken(ServerRef{Name: "prod"}))
	_, err := NewFileStore(dir).GetToken(ServerRef{Name: "prod"})
	assert.ErrorIs(t, err, ErrNoToken, "keychain DeleteToken must also drain the file store")
}

// TestDrainTokenWarnsOnUnreachableKeychainButStillDrainsFile: DrainLoginToken
// now shares keyringStore.DeleteToken's XP-6 degrade — an unreachable
// keychain warns rather than turning `auth logout` into a hard failure,
// as long as the file store drains cleanly.
func TestDrainTokenWarnsOnUnreachableKeychainButStillDrainsFile(t *testing.T) {
	keyring.MockInitWithError(errors.New("keychain boom"))
	t.Cleanup(keyring.MockInit) // reset the global provider for later tests
	dir := t.TempDir()
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod"}, "in-file"))

	var buf bytes.Buffer
	orig := logrus.StandardLogger().Out
	logrus.SetOutput(&buf)
	defer logrus.SetOutput(orig)

	from, _, err := DrainToken(dir, ServerRef{Name: "prod", URL: "https://prod.ex"}, "file")
	require.NoError(t, err, "an unreachable-keychain delete error must not fail logout when the file store drains")
	assert.Contains(t, from, "file", "file backend must still be drained")
	_, gerr := NewFileStore(dir).GetToken(ServerRef{Name: "prod"})
	assert.ErrorIs(t, gerr, ErrNoToken, "file token must be gone even when the keychain was unreachable")
	assert.Contains(t, buf.String(), "keychain unreachable", "the unreachable keychain must still be visibly warned about")
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

// TestKeyringUnreachableGetReadsThroughToFile: a backend-unreachable error
// (NOT ErrNotFound) with a valid token in the file store must return the
// file token — the headless-with-keychain-pin case (XP-6).
func TestKeyringUnreachableGetReadsThroughToFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, NewFileStore(dir).SetToken(ServerRef{Name: "prod"}, "file-token"))
	keyring.MockInitWithError(errors.New("dbus: no session bus"))
	t.Cleanup(keyring.MockInit)

	s := newPinnedKeyringStore(dir, true)
	tok, err := s.GetToken(ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "file-token", tok)
}

// TestKeyringUnreachableGetDoubleMissKeepsKeyringError: when the file
// store ALSO misses, the surfaced error must be the keyring transport
// error (the token may exist in the unreachable keychain) — not
// ErrNoToken, which would misread as "logged out".
func TestKeyringUnreachableGetDoubleMissKeepsKeyringError(t *testing.T) {
	dir := t.TempDir()
	keyring.MockInitWithError(errors.New("dbus: no session bus"))
	t.Cleanup(keyring.MockInit)

	s := newPinnedKeyringStore(dir, true)
	_, err := s.GetToken(ServerRef{Name: "prod"})
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNoToken)
	assert.Contains(t, err.Error(), "no session bus")
}

// TestKeyringUnreachableGetSurfacesFileStoreFailure: an unreachable
// keychain PLUS a broken credentials.yaml must surface both problems —
// the file-store failure must not be masked as a keychain-only issue.
func TestKeyringUnreachableGetSurfacesFileStoreFailure(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "credentials.yaml"), []byte("{not yaml: ["), 0o600))
	keyring.MockInitWithError(errors.New("dbus: no session bus"))
	t.Cleanup(keyring.MockInit)

	s := newPinnedKeyringStore(dir, true)
	_, err := s.GetToken(ServerRef{Name: "prod"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no session bus")
	assert.Contains(t, err.Error(), "file credential store")
}

// TestKeyringUnreachableDeleteDrainsFile: delete under an unreachable
// keychain still drains the file store and reports success (spec XP-6:
// same as a miss; logout's DrainLoginToken independently surfaces
// keyring errors) — and WARNS visibly, naming the unreachable keychain
// and the file-only drain, so the undeletable keychain copy (if any) is
// never silent.
func TestKeyringUnreachableDeleteDrainsFile(t *testing.T) {
	dir := t.TempDir()
	fs := NewFileStore(dir)
	require.NoError(t, fs.SetToken(ServerRef{Name: "prod"}, "file-token"))
	keyring.MockInitWithError(errors.New("dbus: no session bus"))
	t.Cleanup(keyring.MockInit)

	var buf bytes.Buffer
	orig := logrus.StandardLogger().Out
	logrus.SetOutput(&buf)
	defer logrus.SetOutput(orig)

	s := newPinnedKeyringStore(dir, true)
	require.NoError(t, s.DeleteToken(ServerRef{Name: "prod"}))
	_, err := fs.GetToken(ServerRef{Name: "prod"})
	assert.ErrorIs(t, err, ErrNoToken)
	assert.Contains(t, buf.String(), "keychain unreachable")
	assert.Contains(t, buf.String(), "file store only")
}

// TestKeyringSetDataTooBigIsNotMisdiagnosedAsUnreachable: an oversized
// token must surface go-keyring's own message plainly, not the
// "unreachable... locked keychain? no D-Bus session?" remedy — a
// data-size failure is not a connectivity one (XP-6 fix round, finding 6).
func TestKeyringSetDataTooBigIsNotMisdiagnosedAsUnreachable(t *testing.T) {
	dir := t.TempDir()
	keyring.MockInitWithError(keyring.ErrSetDataTooBig)
	t.Cleanup(keyring.MockInit)

	s := newPinnedKeyringStore(dir, true)
	err := s.SetToken(ServerRef{Name: "prod"}, "oversized-token")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "unreachable")
	assert.Contains(t, err.Error(), keyring.ErrSetDataTooBig.Error())
}

// TestKeyringUnreachableSetRefusesWithRemedy: a login write under an
// unreachable keychain must hard-fail with the env-var remedy and must
// NOT create a plaintext credentials file — the anti-silent-downgrade
// guarantee (XP-6, decided: guarantee wins on the write path).
func TestKeyringUnreachableSetRefusesWithRemedy(t *testing.T) {
	dir := t.TempDir()
	keyring.MockInitWithError(errors.New("dbus: no session bus"))
	t.Cleanup(keyring.MockInit)

	s := newPinnedKeyringStore(dir, true)
	err := s.SetToken(ServerRef{Name: "prod"}, "new-token")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unreachable")
	assert.Contains(t, err.Error(), EnvCredentialStore+"=file")
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
	want := CanonicalServerHost("https://host")
	require.NotEmpty(t, want)
	for _, s := range spellings {
		assert.Equal(t, want, CanonicalServerHost(s), "spelling %q must canonicalize to %q", s, want)
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
	want := CanonicalServerHost("http://host")
	require.NotEmpty(t, want)
	for _, s := range spellings {
		assert.Equal(t, want, CanonicalServerHost(s), "spelling %q must canonicalize to %q", s, want)
	}
}
