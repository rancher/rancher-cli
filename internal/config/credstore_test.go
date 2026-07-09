package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
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
		_, err := s.GetToken("acme")
		assert.ErrorIs(t, err, ErrNoToken)
	})
	t.Run("set then get", func(t *testing.T) {
		require.NoError(t, s.SetToken("acme", "tok-abc:secret"))
		got, err := s.GetToken("acme")
		require.NoError(t, err)
		assert.Equal(t, "tok-abc:secret", got)
	})
	t.Run("delete", func(t *testing.T) {
		require.NoError(t, s.SetToken("acme", "x"))
		require.NoError(t, s.DeleteToken("acme"))
		_, err := s.GetToken("acme")
		assert.ErrorIs(t, err, ErrNoToken)
	})
}

func TestOpenStoreFilePolicy(t *testing.T) {
	s, err := OpenStore("file", t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.SetToken("acme", "y"))
	got, err := s.GetToken("acme")
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
	defer func() { keyringUsable = orig }()

	if _, err := OpenStore("auto", dir); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore("auto", dir); err != nil {
		t.Fatal(err)
	}
	if probes != 1 {
		t.Errorf("probes = %d, want 1 (second run must read the pin)", probes)
	}
	if got := ReadPin(dir); got != "file" {
		t.Errorf("pin = %q, want file", got)
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
	orig := logrus.StandardLogger().Out
	var buf bytes.Buffer
	logrus.SetOutput(&buf)
	defer logrus.SetOutput(orig)

	// Loose (0644): warns once, then deduped for the same path.
	looseDir := t.TempDir()
	loose := NewFileStore(looseDir)
	require.NoError(t, loose.SetToken("acme", "id:sec"))
	require.NoError(t, os.Chmod(filepath.Join(looseDir, "credentials.yaml"), 0o644))
	_, err := loose.GetToken("acme")
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "group/world-readable", "0644 credentials must warn")

	buf.Reset()
	_, err = loose.GetToken("acme")
	require.NoError(t, err)
	assert.NotContains(t, buf.String(), "group/world-readable", "warning must fire once per path per process")

	// Tight (0600), fresh path: silent.
	buf.Reset()
	tight := NewFileStore(t.TempDir())
	require.NoError(t, tight.SetToken("acme", "id:sec")) // written 0600
	_, err = tight.GetToken("acme")
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
	s := NewKeyringStore(dir)
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
	if err := NewFileStore(dir).SetToken("prod", "id:secret"); err != nil {
		t.Fatal(err)
	}
	s := NewKeyringStore(dir)
	tok, err := s.GetToken("prod")
	if err != nil || tok != "id:secret" {
		t.Fatalf("read-through: got %q, %v", tok, err)
	}
}

func TestDrainTokenRemovesFromBothBackends(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	_ = keyring.Set(keyringService, "prod", "in-keychain")
	_ = NewFileStore(dir).SetToken("prod", "in-file")
	from, err := DrainToken(dir, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(from) != 2 {
		t.Errorf("deletedFrom = %v, want both backends", from)
	}
	if _, err := keyring.Get(keyringService, "prod"); err == nil {
		t.Error("token still in keychain")
	}
	if _, err := NewFileStore(dir).GetToken("prod"); err == nil {
		t.Error("token still in file store")
	}
}

func TestDrainTokenFailsOnUnreadableFileStore(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	// corrupt credentials.yaml: GetToken returns a non-ErrNoToken error
	if err := os.WriteFile(filepath.Join(dir, "credentials.yaml"), []byte("\t: not yaml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := DrainToken(dir, "prod")
	if err == nil {
		t.Fatal("DrainToken must fail, not report success, when the file store is unreadable (a live token may remain)")
	}
}

func TestKeyringDeleteTokenDrainsFileStore(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	// Token lives only in the file store (e.g. written under an earlier file pin).
	require.NoError(t, NewFileStore(dir).SetToken("prod", "in-file"))
	s := NewKeyringStore(dir)
	require.NoError(t, s.DeleteToken("prod"))
	_, err := NewFileStore(dir).GetToken("prod")
	assert.ErrorIs(t, err, ErrNoToken, "keychain DeleteToken must also drain the file store")
}

func TestDrainTokenJoinsKeychainErrorButStillDrainsFile(t *testing.T) {
	keyring.MockInitWithError(errors.New("keychain boom"))
	t.Cleanup(keyring.MockInit) // reset the global provider for later tests
	dir := t.TempDir()
	require.NoError(t, NewFileStore(dir).SetToken("prod", "in-file"))

	from, err := DrainToken(dir, "prod")
	require.Error(t, err, "a keychain delete error must surface, not be swallowed")
	assert.Contains(t, from, "file", "file backend must still be drained despite the keychain error")
	_, gerr := NewFileStore(dir).GetToken("prod")
	assert.ErrorIs(t, gerr, ErrNoToken, "file token must be gone even when the keychain errored")
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
