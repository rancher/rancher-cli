package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// revokeFixture is a directory with three entries at one server and one
// record of each kind: prod's own credential, a call that identified prod
// answered by the plugin login entry, and a call that identified none.
func revokeFixture(t *testing.T) (string, *Config) {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{TokenReuse: true, Servers: map[string]Server{
		"prod":                {URL: "https://r.ex", CacheBindingID: "p1"},
		"dev":                 {URL: "https://r.ex", CacheBindingID: "d1"},
		"exec-login-r.ex":     {URL: "https://r.ex", CacheBindingID: "x1", Provenance: clierr.ProvenanceExecLogin},
		"elsewhere":           {URL: "https://other.ex", CacheBindingID: "e1"},
		"exec-login-other.ex": {URL: "https://other.ex", CacheBindingID: "o1", Provenance: clierr.ProvenanceExecLogin},
	}}
	canon := CanonicalDir(dir)
	store := NewFileStore(dir)
	exp := time.Now().Add(time.Hour)
	ref := &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-own"}
	require.NoError(t, store.SetCachedCred("own", "ext/token-own:s", exp, "prod", "https://r.ex", "c-1", CacheBindingFor(canon, cfg, "prod").WithCreatedToken(ref, "user-a")))
	require.NoError(t, store.SetCachedCred("identified", "t-id", exp, "prod", "https://r.ex", "c-2", CacheBindingForIdentified(canon, cfg, "https://r.ex", "prod", "", "")))
	require.NoError(t, store.SetCachedCred("server", "t-srv", exp, "kube", "https://r.ex", "c-3", CacheBindingForServer(canon, cfg, "https://r.ex", false)))
	return dir, cfg
}

// TestHoldCachedCredsReadsEveryRecordUnderTheLock asserts the read hold:
// every record with its token, ref, owner, kind and the entries its binding
// names, read while the token-cache lock is held, and nothing deleted.
func TestHoldCachedCredsReadsEveryRecordUnderTheLock(t *testing.T) {
	dir, _ := revokeFixture(t)
	var got []CachedRecord
	require.NoError(t, NewFileStore(dir).HoldCachedCreds(func(recs []CachedRecord) error {
		got = recs
		locked, err := flock.New(filepath.Join(dir, "token-cache.lock")).TryLock()
		require.NoError(t, err)
		assert.False(t, locked, "the token-cache lock is held while fn runs")
		return nil
	}))
	require.Len(t, got, 3)
	byKey := map[string]CachedRecord{}
	for _, r := range got {
		byKey[r.Key] = r
	}
	own := byKey["own"]
	assert.Equal(t, CacheRecordOwnCredential, own.Kind)
	assert.Equal(t, "prod", own.Identified)
	assert.False(t, own.Listed)
	assert.Equal(t, "ext/token-own:s", own.Token)
	assert.Equal(t, &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-own"}, own.TokenRef)
	assert.Equal(t, "user-a", own.TokenUserID)

	id := byKey["identified"]
	assert.Equal(t, CacheRecordIdentified, id.Kind)
	assert.True(t, id.Listed)
	assert.Equal(t, []string{"exec-login-r.ex"}, id.Entries)
	assert.Nil(t, id.TokenRef)

	srv := byKey["server"]
	assert.Equal(t, CacheRecordServer, srv.Kind)
	assert.Empty(t, srv.Identified)
	assert.Equal(t, []string{"dev", "exec-login-r.ex", "prod"}, srv.Entries)

	creds, err := NewFileStore(dir).ListCachedCreds()
	require.NoError(t, err)
	assert.Len(t, creds, 3, "the hold deletes nothing")
}

// TestHoldCachedCredsFailsOnAnUnreadableCache asserts that a cache file
// that cannot be parsed fails the hold before fn runs.
func TestHoldCachedCredsFailsOnAnUnreadableCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "token-cache.yaml"), []byte("{not yaml"), 0o600))
	called := false
	err := NewFileStore(dir).HoldCachedCreds(func([]CachedRecord) error { called = true; return nil })
	require.Error(t, err)
	assert.False(t, called)
}

// TestDeleteCachedCredsIfUnchangedComparesTheToken asserts the second
// hold's compare: a record that still holds the token read is deleted, one
// rewritten since is kept and reported, and one already gone is neither.
func TestDeleteCachedCredsIfUnchangedComparesTheToken(t *testing.T) {
	dir, _ := revokeFixture(t)
	deleted, changed, err := NewFileStore(dir).DeleteCachedCredsIfUnchanged(map[string]string{
		"own": "ext/token-own:s", "identified": "an older token", "gone": "t",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"own"}, deleted)
	assert.Equal(t, []string{"identified"}, changed)
	creds, err := NewFileStore(dir).ListCachedCreds()
	require.NoError(t, err)
	var keys []string
	for _, c := range creds {
		keys = append(keys, c.Key)
	}
	assert.ElementsMatch(t, []string{"identified", "server"}, keys)
}

// TestEveryStoreForwardsTheRevocationHolds asserts that the keychain,
// helper and lazy stores run both holds on the directory's cache file.
func TestEveryStoreForwardsTheRevocationHolds(t *testing.T) {
	keyring.MockInit()
	for name, open := range map[string]func(dir string) CredStore{
		"keychain": func(dir string) CredStore { return newPinnedKeyringStore(dir, false) },
		"helper":   func(dir string) CredStore { return newExecStore("fake", dir) },
		"lazy": func(dir string) CredStore {
			lazy, err := NewLazyStore(&Config{CredentialStore: "file"}, dir)
			require.NoError(t, err)
			return lazy
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := revokeFixture(t)
			store := open(dir)
			var n int
			require.NoError(t, store.HoldCachedCreds(func(recs []CachedRecord) error { n = len(recs); return nil }))
			assert.Equal(t, 3, n)
			deleted, _, err := store.DeleteCachedCredsIfUnchanged(map[string]string{"server": "t-srv"})
			require.NoError(t, err)
			assert.Equal(t, []string{"server"}, deleted)
		})
	}
}

// TestInDeleteScopeFollowsTheBindingRule asserts which records deleting an
// entry's credential deletes: those of its server whose binding names it,
// under any id, and those naming no entry that still exists; a record
// another entry still reads, one of another server and a record with no
// server stay.
func TestInDeleteScopeFollowsTheBindingRule(t *testing.T) {
	t.Parallel()
	_, cfg := revokeFixture(t)
	for _, tc := range []struct {
		name  string
		rec   CachedRecord
		entry string
		want  bool
	}{
		{"its own credential", CachedRecord{Server: "https://r.ex", Kind: CacheRecordOwnCredential, Identified: "prod"}, "prod", true},
		{"another entry's own credential", CachedRecord{Server: "https://r.ex", Kind: CacheRecordOwnCredential, Identified: "dev"}, "prod", false},
		{"a plugin answer for it", CachedRecord{Server: "https://r.ex", Kind: CacheRecordIdentified, Identified: "prod", Listed: true, Entries: []string{"exec-login-r.ex"}}, "prod", true},
		{"an answer by the plugin entry being deleted", CachedRecord{Server: "https://r.ex", Kind: CacheRecordIdentified, Identified: "dev", Listed: true, Entries: []string{"exec-login-r.ex"}}, "exec-login-r.ex", true},
		{"a server record listing it", CachedRecord{Server: "https://r.ex", Kind: CacheRecordServer, Listed: true, Entries: []string{"dev", "prod"}}, "prod", true},
		{"a server record of other entries", CachedRecord{Server: "https://r.ex", Kind: CacheRecordServer, Listed: true, Entries: []string{"dev"}}, "prod", false},
		{"a record of removed entries", CachedRecord{Server: "https://r.ex", Kind: CacheRecordServer, Listed: true, Entries: []string{"gone"}}, "prod", true},
		{"a removed entry's own credential", CachedRecord{Server: "https://r.ex", Kind: CacheRecordOwnCredential, Identified: "gone"}, "prod", true},
		{"a bare-install record", CachedRecord{Server: "https://r.ex", Kind: CacheRecordServer, Listed: true}, "prod", true},
		{"a record of another server", CachedRecord{Server: "https://other.ex", Kind: CacheRecordOwnCredential, Identified: "prod"}, "prod", false},
		{"a record with no server", CachedRecord{Kind: CacheRecordOwnCredential, Identified: "prod"}, "prod", false},
		{"a record whose server carries only the host", CachedRecord{Server: "r.ex", Identified: "prod"}, "prod", true},
		{"an entry the config does not have", CachedRecord{Server: "https://r.ex", Identified: "gone"}, "gone", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, cfg.InDeleteScope(tc.rec, tc.entry))
		})
	}
}
