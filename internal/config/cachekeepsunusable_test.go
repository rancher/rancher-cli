package config

import (
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestARecordBoundToAnEntryThatCannotBeUsedIsServedToNobodyAndKept asserts
// that a live cache record that names an entry whose URL cannot be used, as the
// identified entry or in a list it is bound to, is not served and not deleted
// by a read: the token it caches can be revoked only through the record. It
// holds for each kind of record, with the entry unusable after the record was
// written (the form an earlier build left). An expired record, and a record
// whose entry is gone, are still pruned.
func TestARecordBoundToAnEntryThatCannotBeUsedIsServedToNobodyAndKept(t *testing.T) {
	const server = "https://r.ex"
	identity := CanonicalServerIdentity(server)
	base := func(reuse bool) *Config {
		c := &Config{TokenReuse: reuse, Servers: map[string]Server{
			"y":            {URL: server, CacheBindingID: "b1", TokenUserID: "u-a"},
			"x":            {URL: server + "/p", CacheBindingID: "b2", TokenUserID: "u-a"},
			"exec-login-o": {URL: server, CacheBindingID: "b3", TokenUserID: "u-a", Provenance: clierr.ProvenanceExecLogin},
			"exec-login-x": {URL: server + "/p", CacheBindingID: "b4", TokenUserID: "u-a", Provenance: clierr.ProvenanceExecLogin},
		}}
		if reuse {
			// The plugin login entry of a usable URL is not there: with two the
			// lookup refuses the call and no record is written.
			delete(c.Servers, "exec-login-o")
		}
		return c
	}
	for _, tc := range []struct {
		name  string
		reuse bool
		key   string
		bind  func(dir string, cfg *Config) CacheBinding
	}{
		{"a record of a call that identifies no entry", false, CacheKeyFor("kube", identity, "c-1", "", ""),
			func(dir string, cfg *Config) CacheBinding { return CacheBindingForServer(dir, cfg, server, false) }},
		{"a record of a call with a user", false, CacheKeyFor("kube", identity, "c-1", "u-a", ""),
			func(dir string, cfg *Config) CacheBinding { return CacheBindingForOwner(dir, cfg, server, "u-a", "") }},
		{"a record bound to the plugin login entries of an identified entry", true, CacheKeyFor("y", identity, "c-1", "", ""),
			func(dir string, cfg *Config) CacheBinding {
				return CacheBindingForIdentified(dir, cfg, server, "y", "", "")
			}},
		{"a record of the identified entry's own credential", false, CacheKeyFor("x", identity, "c-1", "", ""),
			func(dir string, cfg *Config) CacheBinding { return CacheBindingFor(dir, cfg, "x") }},
	} {
		for _, expired := range []bool{false, true} {
			name := tc.name
			if expired {
				name += ", expired"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				cfg := base(tc.reuse)
				cfg.SetPath(FilePath(dir))
				require.NoError(t, cfg.Write())
				loaded, err := Load(FilePath(dir))
				require.NoError(t, err)
				expiry := time.Now().Add(time.Hour)
				if expired {
					expiry = time.Now().Add(-time.Hour)
				}
				store := NewFileStore(dir)
				ref := &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-cch01"}
				require.NoError(t, store.SetCachedCred(tc.key, "ext/token-cch01:secret", expiry, "kube", server, "c-1",
					tc.bind(CanonicalDir(dir), loaded).WithCreatedToken(ref, "u-a")))

				flipped := base(tc.reuse)
				x := flipped.Servers["x"]
				x.URL = server + "/p@x"
				flipped.Servers["x"] = x
				xp := flipped.Servers["exec-login-x"]
				xp.URL = server + "/p@x"
				flipped.Servers["exec-login-x"] = xp
				flipped.SetPath(FilePath(dir))
				require.NoError(t, flipped.Write())

				_, _, err = store.GetCachedCred(tc.key, testAnchor(t, dir))
				require.ErrorIs(t, err, ErrNoToken, "the record is served to nobody")
				recs, lerr := readCacheRecords(store)
				require.NoError(t, lerr)
				if expired {
					assert.Empty(t, recs, "an expired record is pruned")
					return
				}
				assert.Contains(t, recs, tc.key, "the record is kept")
			})
		}
	}

	t.Run("a record whose identified entry is gone is pruned", func(t *testing.T) {
		dir := t.TempDir()
		cfg := base(false)
		cfg.SetPath(FilePath(dir))
		require.NoError(t, cfg.Write())
		loaded, err := Load(FilePath(dir))
		require.NoError(t, err)
		key := CacheKeyFor("y", identity, "c-1", "", "")
		store := NewFileStore(dir)
		require.NoError(t, store.SetCachedCred(key, "tok", time.Now().Add(time.Hour), "y", server, "c-1", CacheBindingFor(CanonicalDir(dir), loaded, "y")))
		delete(cfg.Servers, "y")
		require.NoError(t, cfg.Write())
		_, _, err = store.GetCachedCred(key, testAnchor(t, dir))
		require.ErrorIs(t, err, ErrNoToken)
		recs, lerr := readCacheRecords(store)
		require.NoError(t, lerr)
		assert.Empty(t, recs, "another stale reason still prunes")
	})
}

// readCacheRecords is the keys the cache holds, read with no judgement.
func readCacheRecords(f CredStore) (map[string]cachedCred, error) {
	return f.(*fileStore).readCache()
}

// TestAWriteMovesALiveRecordOfAnUnusableEntryToAKeyNoLookupReads asserts that a
// write under the key of a live record that names an entry whose URL cannot
// be used keeps that record, in the same write, under a key derived from its
// token ref, and that a record of a usable entry, an expired one and a write of
// the same token are replaced as before.
func TestAWriteMovesALiveRecordOfAnUnusableEntryToAKeyNoLookupReads(t *testing.T) {
	const server = "https://r.ex"
	identity := CanonicalServerIdentity(server)
	key := CacheKeyFor("kube", identity, "c-1", "", "")
	ref := &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-cch01"}
	cfgOf := func(xURL string) *Config {
		return &Config{Servers: map[string]Server{
			"x": {URL: xURL, CacheBindingID: "b2"},
			"y": {URL: server, CacheBindingID: "b1"},
		}}
	}
	seed := func(t *testing.T, expiry time.Time) (dir string, store CredStore) {
		t.Helper()
		dir = t.TempDir()
		cfg := cfgOf(server + "/p")
		cfg.SetPath(FilePath(dir))
		require.NoError(t, cfg.Write())
		loaded, err := Load(FilePath(dir))
		require.NoError(t, err)
		store = NewFileStore(dir)
		require.NoError(t, store.SetCachedCred(key, "ext/token-cch01:secret", expiry, "kube", server, "c-1",
			CacheBindingForServer(CanonicalDir(dir), loaded, server, false).WithCreatedToken(ref, "u-a")))
		return dir, store
	}
	write := func(t *testing.T, dir string, store CredStore, xURL, token string) {
		t.Helper()
		cfg := cfgOf(xURL)
		cfg.SetPath(FilePath(dir))
		require.NoError(t, cfg.Write())
		loaded, err := Load(FilePath(dir))
		require.NoError(t, err)
		require.NoError(t, store.SetCachedCred(key, token, time.Now().Add(time.Hour), "kube", server, "c-1",
			CacheBindingForServer(CanonicalDir(dir), loaded, server, false).WithCreatedToken(&clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-new"}, "u-a")))
	}
	t.Run("the record of an unusable entry is kept apart", func(t *testing.T) {
		dir, store := seed(t, time.Now().Add(time.Hour))
		write(t, dir, store, server+"/p@x", "ext/token-new:secret")
		recs, err := readCacheRecords(store)
		require.NoError(t, err)
		require.Len(t, recs, 2)
		assert.Equal(t, "ext/token-new:secret", recs[key].Token)
		kept := recs[KeptTokenKey(server, *ref)]
		assert.Equal(t, "ext/token-cch01:secret", kept.Token, "the replaced record is kept under its token's key")
		assert.True(t, strings.HasPrefix(KeptTokenKey(server, *ref), KeptTokenKeyPrefix))
		infos, err := store.ListCachedCreds()
		require.NoError(t, err)
		assert.Len(t, infos, 2, "both are listed")
		_, _, gerr := store.GetCachedCred(KeptTokenKey(server, *ref), testAnchor(t, dir))
		require.ErrorIs(t, gerr, ErrNoToken, "no lookup serves it")
		recs, err = readCacheRecords(store)
		require.NoError(t, err)
		assert.Len(t, recs, 2, "a read keeps it")
	})
	t.Run("a record of usable entries is replaced", func(t *testing.T) {
		dir, store := seed(t, time.Now().Add(time.Hour))
		write(t, dir, store, server+"/p", "ext/token-new:secret")
		recs, err := readCacheRecords(store)
		require.NoError(t, err)
		assert.Len(t, recs, 1)
	})
	t.Run("an expired record is replaced", func(t *testing.T) {
		dir, store := seed(t, time.Now().Add(-time.Hour))
		write(t, dir, store, server+"/p@x", "ext/token-new:secret")
		recs, err := readCacheRecords(store)
		require.NoError(t, err)
		assert.Len(t, recs, 1)
	})
	t.Run("a write of the same token replaces it", func(t *testing.T) {
		dir, store := seed(t, time.Now().Add(time.Hour))
		write(t, dir, store, server+"/p@x", "ext/token-cch01:secret")
		recs, err := readCacheRecords(store)
		require.NoError(t, err)
		assert.Len(t, recs, 1)
	})
}

// TestTheKeptKeyOfARecordHoldsItsServerAndNeverReplacesAnotherToken asserts
// that two live records of the same token kind and name at two servers, both
// replaced by a write, are both kept, and that a second record of the same
// server, kind and name with another token is kept under a key with a suffix,
// not over the first.
func TestTheKeptKeyOfARecordHoldsItsServerAndNeverReplacesAnotherToken(t *testing.T) {
	ref := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-cch01"}
	servers := []string{"https://r.ex", "https://s.ex"}
	setup := func(t *testing.T) (string, CredStore) {
		t.Helper()
		dir := t.TempDir()
		cfg := &Config{Servers: map[string]Server{}}
		for i, s := range servers {
			cfg.Servers[[]string{"x", "u"}[i]] = Server{URL: s + "/p", CacheBindingID: "bx" + string(rune('1'+i))}
			cfg.Servers[[]string{"y", "v"}[i]] = Server{URL: s, CacheBindingID: "by" + string(rune('1'+i))}
		}
		cfg.SetPath(FilePath(dir))
		require.NoError(t, cfg.Write())
		return dir, NewFileStore(dir)
	}
	seed := func(t *testing.T, dir string, store CredStore, server, cluster, token string) string {
		t.Helper()
		loaded, err := Load(FilePath(dir))
		require.NoError(t, err)
		key := CacheKeyFor("kube", CanonicalServerIdentity(server), cluster, "", "")
		require.NoError(t, store.SetCachedCred(key, token, time.Now().Add(time.Hour), "kube", server, cluster,
			CacheBindingForServer(CanonicalDir(dir), loaded, server, false).WithCreatedToken(&ref, "u-a")))
		return key
	}
	breakEntries := func(t *testing.T, dir string, names ...string) {
		t.Helper()
		cfg, err := Load(FilePath(dir))
		require.NoError(t, err)
		for _, n := range names {
			e := cfg.Servers[n]
			e.URL += "@x"
			cfg.Servers[n] = e
		}
		require.NoError(t, cfg.Write())
	}
	replace := func(t *testing.T, dir string, store CredStore, server, cluster, token string) {
		t.Helper()
		loaded, err := Load(FilePath(dir))
		require.NoError(t, err)
		key := CacheKeyFor("kube", CanonicalServerIdentity(server), cluster, "", "")
		require.NoError(t, store.SetCachedCred(key, token, time.Now().Add(time.Hour), "kube", server, cluster,
			CacheBindingForServer(CanonicalDir(dir), loaded, server, false).WithCreatedToken(&clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-new"}, "u-a")))
	}
	t.Run("the same kind and name at two servers", func(t *testing.T) {
		dir, store := setup(t)
		seed(t, dir, store, servers[0], "c-1", "tok-r")
		seed(t, dir, store, servers[1], "c-1", "tok-s")
		breakEntries(t, dir, "x", "u")
		replace(t, dir, store, servers[0], "c-1", "new-r")
		replace(t, dir, store, servers[1], "c-1", "new-s")
		recs, err := readCacheRecords(store)
		require.NoError(t, err)
		require.Len(t, recs, 4)
		assert.Equal(t, "tok-r", recs[KeptTokenKey(servers[0], ref)].Token)
		assert.Equal(t, "tok-s", recs[KeptTokenKey(servers[1], ref)].Token)
		assert.NotEqual(t, KeptTokenKey(servers[0], ref), KeptTokenKey(servers[1], ref))
	})
	t.Run("the same token moved twice keeps one key", func(t *testing.T) {
		dir, store := setup(t)
		seed(t, dir, store, servers[0], "c-1", "tok-1")
		breakEntries(t, dir, "x")
		replace(t, dir, store, servers[0], "c-1", "new-1")
		cfg, err := Load(FilePath(dir))
		require.NoError(t, err)
		x := cfg.Servers["x"]
		x.URL = strings.TrimSuffix(x.URL, "@x")
		cfg.Servers["x"] = x
		require.NoError(t, cfg.Write())
		seed(t, dir, store, servers[0], "c-1", "tok-1")
		breakEntries(t, dir, "x")
		replace(t, dir, store, servers[0], "c-1", "new-2")
		recs, err := readCacheRecords(store)
		require.NoError(t, err)
		var kept []string
		for k := range recs {
			if strings.HasPrefix(k, KeptTokenKeyPrefix) {
				kept = append(kept, k)
			}
		}
		assert.Equal(t, []string{KeptTokenKey(servers[0], ref)}, kept, "one key for one token")
	})
	t.Run("another token of the same server, kind and name", func(t *testing.T) {
		dir, store := setup(t)
		seed(t, dir, store, servers[0], "c-1", "tok-1")
		seed(t, dir, store, servers[0], "c-2", "tok-2")
		breakEntries(t, dir, "x")
		replace(t, dir, store, servers[0], "c-1", "new-1")
		replace(t, dir, store, servers[0], "c-2", "new-2")
		recs, err := readCacheRecords(store)
		require.NoError(t, err)
		var kept []string
		for k, r := range recs {
			if strings.HasPrefix(k, KeptTokenKeyPrefix) {
				kept = append(kept, r.Token)
			}
		}
		assert.ElementsMatch(t, []string{"tok-1", "tok-2"}, kept, "both are kept")
	})
}
