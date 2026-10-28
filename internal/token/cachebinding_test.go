package token

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// anchorFor is the anchor the root hook builds in production: the binding
// stamp read first, the config loaded after it. A config that will not
// parse leaves the anchor without one, which is exactly what the root hook
// stores in that case and is what makes every cache hit a miss.
// Every invocation takes a fresh one: a process that started before a
// login cannot judge records against what the login wrote, and reusing
// one anchor across two calls would hide exactly that.
func anchorFor(dir string) *config.CacheAnchor {
	anchor := config.NewCacheAnchor(dir)
	cfg, lerr := config.Load(config.FilePath(dir))
	if lerr != nil {
		return anchor
	}
	return anchor.WithConfig(cfg)
}

// newTokenCommandForTest builds `auth get-token` with an anchor for dir,
// the pair the CLI layer hands it in production. A command built without
// one treats every cache record as unjudgeable and misses, which is the
// right answer there and the wrong one in a test about caching.
func newTokenCommandForTest(t *testing.T, dir string, store config.CredStore) runtime.Command {
	t.Helper()
	return NewTokenCommand(dir, store, anchorFor(dir))
}

// testBinding is the binding a record written in dir for a call named
// name carries, computed the way the production write computes it from
// that load: the identified entry alone when --name identifies one, whose
// own credential the test takes to have answered, else what the lookup
// of a call that identifies none reads.
//
// An entry a test seeded by hand carries no binding id, exactly as an
// entry whose last login predates the id does, and a record made from it
// carries none either: the pair is equal and the record is served.
func testBinding(t *testing.T, dir, name, server, cluster string) config.CacheBinding {
	t.Helper()
	return testBindingFor(t, dir, name, server, cluster, "")
}

// testBindingFor is testBinding for a call with --user-id userID.
func testBindingFor(t *testing.T, dir, name, server, cluster, userID string) config.CacheBinding {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	if err != nil {
		return config.CacheBindingForTest(config.CanonicalDir(dir), "", "")
	}
	return config.CacheBindingForSeedOf(config.CanonicalDir(dir), cfg, name, server, cluster, userID, "")
}

// seedCache writes a cache record the way a real invocation would, bound
// to what the lookup for the call reads.
func seedCache(t *testing.T, dir string, store config.CredStore, key, token string, expiry time.Time, name, server, cluster string) error {
	t.Helper()
	return seedCacheFor(t, dir, store, key, token, expiry, name, server, cluster, "")
}

// seedCacheFor is seedCache for a call with --user-id userID.
func seedCacheFor(t *testing.T, dir string, store config.CredStore, key, token string, expiry time.Time, name, server, cluster, userID string) error {
	t.Helper()
	return store.SetCachedCred(key, token, expiry, name, server, cluster, testBindingFor(t, dir, name, server, cluster, userID))
}

// TestCopiedDirectoryDoesNotServeItsSourcesCacheRecords asserts what the
// directory path in a record buys. Copying a config directory copies its
// exec-credential cache with it, and those records hold live
// cluster-scoped credentials for whoever the source directory was logged
// in as. The copy names a different path, so every one of them is a miss
// there and the copy derives its own.
func TestCopiedDirectoryDoesNotServeItsSourcesCacheRecords(t *testing.T) {
	source := t.TempDir()
	store := config.NewFileStore(source)
	key := config.CacheKey("prod", config.CanonicalServerIdentity("https://r.ex"), "c-1")
	require.NoError(t, seedCache(t, source, store, key, "source-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))

	// The copy carries the same files, cache included.
	copyDir := t.TempDir()
	copyDirectory(t, source, copyDir)

	inSource := tokenCmd{dir: source, store: store, anchor: anchorFor(source)}
	_, _, _, hit := inSource.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
	assert.True(t, hit, "the directory that made the record still serves it")

	inCopy := tokenCmd{dir: copyDir, store: config.NewFileStore(copyDir), anchor: anchorFor(copyDir)}
	_, _, _, hit = inCopy.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
	assert.False(t, hit, "the copy never serves the credential its source cached")
}

// TestBareInstallServesItsOwnCacheRecords asserts the hard constraint: a
// directory with no config.yaml at all - the state the exec plugin is
// installed into - caches and serves normally. An absent file is not a
// broken one; it reads as an empty config, and a record made while the
// directory had no entry for the server is a hit while that holds.
func TestBareInstallServesItsOwnCacheRecords(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	key := config.CacheKey("prod", config.CanonicalServerIdentity("https://r.ex"), "c-1")
	require.NoError(t, seedCache(t, dir, store, key, "bare-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))
	require.NoFileExists(t, config.FilePath(dir))
	require.NoFileExists(t, config.BindingStampPath(dir))

	cmd := tokenCmd{dir: dir, store: store, anchor: anchorFor(dir)}
	_, _, r, hit := cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
	require.True(t, hit, "an absent config.yaml is the bare-install state, not a broken one")
	assert.Equal(t, "bare-tok", r.Data.(*execCredential).Status.Token)

	// Once the directory has an entry for that server, the record no
	// longer says who it belongs to and stops being served.
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd = tokenCmd{dir: dir, store: store, anchor: anchorFor(dir)}
	_, _, _, hit = cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
	assert.False(t, hit, "an entry now decides who the credential belongs to")
}

// TestADamagedBindingStampCostsTheCacheAndNothingElse asserts the rule
// that keeps a directory usable. The binding stamp is read at the start of
// every command, so a stamp file that cannot be parsed must not fail one:
// failing would take down the login and the logout that rewrite it, and it
// would take down the exec plugin, which must keep working
// from a bare install. The cost is the directory's warm cache, paid until
// the next login or logout, and nothing more.
func TestADamagedBindingStampCostsTheCacheAndNothingElse(t *testing.T) {
	for _, content := range []string{"", "not-a-stamp\n", "abcd\n"} {
		dir := t.TempDir()
		store := config.NewFileStore(dir)
		key := config.CacheKey("prod", config.CanonicalServerIdentity("https://r.ex"), "c-1")
		require.NoError(t, seedCache(t, dir, store, key, "cached-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))
		require.NoError(t, os.WriteFile(config.BindingStampPath(dir), []byte(content), 0o600))

		cmd := tokenCmd{dir: dir, store: store, anchor: anchorFor(dir)}
		_, _, _, hit := cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
		assert.False(t, hit, "a record that cannot be judged is not served")

		// The record survives: the reader could not judge it, which is
		// not the same as knowing it is not this directory's.
		require.NoError(t, os.Remove(config.BindingStampPath(dir)))
		cmd = tokenCmd{dir: dir, store: store, anchor: anchorFor(dir)}
		_, _, r, hit := cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
		require.True(t, hit, "with the stamp gone the same record serves again")
		assert.Equal(t, "cached-tok", r.Data.(*execCredential).Status.Token)
	}
}

// TestHotPathOperationCounts is the budget gate. Every per-invocation
// cost here is paid by a cold process and multiplied by the caller's
// fan-out, so the budget is asserted rather than left as prose: one stamp
// read per command and one more per cache hit, no config load, no lock of
// either kind and no keychain call on a hit or a clean miss. A later
// change that adds a third stamp read, a config load, a lock or a
// keychain call to a reader fails this test rather than quietly costing
// every kubectl call.
func TestHotPathOperationCounts(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(dir)
	key := config.CacheKey("prod", config.CanonicalServerIdentity("https://r.ex"), "c-1")
	require.NoError(t, seedCache(t, dir, store, key, "cached-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))

	// count measures only the read. The anchor is built first, outside
	// the measured region, because the root hook builds it once per
	// invocation and its one stamp read is the invocation's, not the
	// reader's.
	// Both lock seams are counted. The config lock and the token-cache
	// flock are separate mechanisms - a reader that prunes or marks a
	// record takes the cache flock and never the config lock - so
	// counting only the config lock would assert nothing about the lock
	// a cache reader can actually reach.
	count := func(t *testing.T, anchor *config.CacheAnchor, run func(*config.CacheAnchor)) (ops map[string]int, keyringOps, locks, flocks int) {
		t.Helper()
		ops = map[string]int{}
		restoreOps := config.SetIdentityObserverForTest(func(op string) { ops[op]++ })
		defer restoreOps()
		config.SetKeyringObserverForTest(func(string) { keyringOps++ })
		defer config.SetKeyringObserverForTest(nil)
		restoreLock := config.SetLockHookForTest(func() { locks++ })
		defer restoreLock()
		restoreFlock := config.SetWaitForFlockHookForTest(func() { flocks++ })
		defer restoreFlock()

		run(anchor)
		return ops, keyringOps, locks, flocks
	}

	t.Run("the plugin's flag fast path", func(t *testing.T) {
		ops, keyringOps, locks, flocks := count(t, anchorFor(dir), func(anchor *config.CacheAnchor) {
			cmd := tokenCmd{dir: dir, store: store, anchor: anchor}
			_, _, _, hit := cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
			require.True(t, hit)
		})
		assert.Equal(t, 1, ops["stampread"],
			"one before the credential is returned; the one before the config load was the invocation's")
		assert.Zero(t, ops["configload"], "a hit adds no config load")
		assert.Zero(t, keyringOps, "a cache hit never opens the keychain")
		assert.Zero(t, locks, "and never takes the config lock")
		assert.Zero(t, flocks, "nor the token-cache flock")
		assert.Zero(t, ops["diropen"], "and never reads an identity record")
		assert.Zero(t, ops["recordread"])
		assert.Zero(t, ops["manifeststat"],
			"a directory that names no store change never opens the manifest")
	})

	t.Run("a hit of each record kind", func(t *testing.T) {
		// Each kind of record is judged against the load the anchor
		// already holds: the identification run again, and for the kinds
		// bound to a list the entries for the URL compared in memory.
		kdir := t.TempDir()
		kcfg := &config.Config{CurrentServer: "prod", TokenReuse: true, Servers: map[string]config.Server{
			"prod":              {URL: "https://r.ex", CacheBindingID: "b1", TokenUserID: "u-abc12"},
			"staging":           {URL: "https://r.ex", CacheBindingID: "b2"},
			"exec-login-plugin": {URL: "https://r.ex", CacheBindingID: "b3", TokenUserID: "u-abc12", Provenance: clierr.ProvenanceExecLogin},
		}}
		kcfg.SetPath(config.FilePath(kdir))
		require.NoError(t, kcfg.Write())
		kstore := config.NewFileStore(kdir)
		canon := config.CanonicalDir(kdir)
		kinds := []struct {
			name       string
			call       string
			user       string
			authConfig string
			shim       bool
			binding    config.CacheBinding
		}{
			{"the identified entry's own credential", "prod-c-1", "", "", false, config.CacheBindingFor(canon, kcfg, "prod")},
			{"a plugin login entry's own credential", "exec-login-plugin-c-1", "", "", false, config.CacheBindingFor(canon, kcfg, "exec-login-plugin")},
			{"another record of an identified call", "staging-c-1", "", "", false, config.CacheBindingForIdentified(canon, kcfg, "https://r.ex", "staging", "", "")},
			{"another record of an identified call with --user-id", "staging-c-1", "u-abc12", "", false, config.CacheBindingForIdentified(canon, kcfg, "https://r.ex", "staging", "u-abc12", "")},
			{"a call that identifies no entry", "kube-c-1", "", "", false, config.CacheBindingForServer(canon, kcfg, "https://r.ex", false)},
			{"a call with --user-id that identifies no entry (the owner record)", "", "u-abc12", "", false, config.CacheBindingForOwner(canon, kcfg, "https://r.ex", "u-abc12", "")},
			{"the legacy shim", config.LegacyCachePrefix + "kube", "", "", true, config.CacheBindingForServer(canon, kcfg, "https://r.ex", true)},
			{"another record of an identified call with --user-id and an auth config", "staging-c-1", "u-abc12", "corp-ldap", false, config.CacheBindingForIdentified(canon, kcfg, "https://r.ex", "staging", "u-abc12", "corp-ldap")},
			{"a call with --user-id and an auth config that identifies no entry", "", "u-abc12", "corp-ldap", false, config.CacheBindingForOwner(canon, kcfg, "https://r.ex", "u-abc12", "corp-ldap")},
			{"the identified entry's own credential with --user-id and an auth config", "prod-c-1", "u-abc12", "corp-ldap", false, config.CacheBindingForLookup(canon, kcfg, "https://r.ex", "prod", "prod", "u-abc12", "corp-ldap", false)},
		}
		for _, k := range kinds {
			t.Run(k.name, func(t *testing.T) {
				kkey := config.CacheKeyFor(k.call, config.CanonicalServerIdentity("https://r.ex"), "c-1", k.user, k.authConfig)
				require.NoError(t, kstore.SetCachedCred(kkey, "tok-"+k.call, time.Now().Add(time.Hour), k.call, "https://r.ex", "c-1", k.binding))
				ops, keyringOps, locks, flocks := count(t, anchorFor(kdir), func(anchor *config.CacheAnchor) {
					var hit bool
					if k.shim {
						_, _, _, hit, _ = shimProbeFlagCache(kdir, k.call, "https://r.ex", "c-1", anchor, ui.Discard())
					} else {
						cmd := tokenCmd{dir: kdir, store: kstore, anchor: anchor}
						_, _, _, hit = cmd.probeFlagCache(k.call, "https://r.ex", "c-1", k.user, k.authConfig, ui.Discard())
					}
					require.True(t, hit)
				})
				assert.Equal(t, map[string]int{"stampread": 1}, ops, "the confirmation only: no config load, no identity or change record read")
				assert.Zero(t, keyringOps)
				assert.Zero(t, locks)
				assert.Zero(t, flocks)
			})
		}
	})

	t.Run("the legacy shim's fast path", func(t *testing.T) {
		ops, keyringOps, locks, flocks := count(t, anchorFor(dir), func(anchor *config.CacheAnchor) {
			_, _, _, hit, _ := shimProbeFlagCache(dir, "prod", "https://r.ex", "c-1", anchor, ui.Discard())
			require.True(t, hit)
		})
		assert.Equal(t, 1, ops["stampread"])
		assert.Zero(t, ops["configload"], "the shim adds no config load of its own")
		assert.Zero(t, keyringOps)
		assert.Zero(t, locks)
		assert.Zero(t, flocks)
		assert.Zero(t, ops["manifeststat"])
	})

	t.Run("the plugin's resolved path", func(t *testing.T) {
		// kubectl names the Rancher proxy it is about to use, so the flag
		// fast path is skipped and the hit is served after the config
		// load that resolves the connection.
		t.Setenv(execInfoEnv, execInfoFor(t, "https://r.ex/k8s/clusters/c-1", nil))
		var served string
		ops, keyringOps, locks, flocks := count(t, anchorFor(dir), func(anchor *config.CacheAnchor) {
			cmd := tokenCmd{dir: dir, store: store, anchor: anchor}
			res, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": "https://r.ex", "name": "prod", "cluster": "c-1"}}, ui.Discard())
			require.NoError(t, err)
			cred, ok := res.Data.(*execCredential)
			require.True(t, ok)
			served = cred.Status.Token
		})
		assert.Equal(t, "cached-tok", served, "the record is served")
		assert.Equal(t, map[string]int{"configload": 1, "stampread": 1}, ops,
			"the connection's config load, then the confirmation; no credential read, no change record stat")
		assert.Zero(t, keyringOps)
		assert.Zero(t, locks)
		assert.Zero(t, flocks)
	})

	t.Run("with --user-id, on both paths", func(t *testing.T) {
		// The user id and the auth config are more key components: the slot
		// they name is read at the same cost, on the flag fast path and on
		// the resolved path.
		loaded, err := config.Load(config.FilePath(dir))
		require.NoError(t, err)
		for _, authConfig := range []string{"", "corp-ldap"} {
			ukey := config.CacheKeyFor("prod", config.CanonicalServerIdentity("https://r.ex"), "c-1", "u-abc12", authConfig)
			require.NoError(t, store.SetCachedCred(ukey, "cached-user-tok-"+authConfig, time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1",
				config.CacheBindingForSeedOf(config.CanonicalDir(dir), loaded, "prod", "https://r.ex", "c-1", "u-abc12", authConfig)))
			for _, resolved := range []bool{false, true} {
				t.Run(map[bool]string{false: "flag fast path", true: "resolved path"}[resolved]+", auth config "+authConfig, func(t *testing.T) {
					want := map[string]int{"stampread": 1}
					if resolved {
						t.Setenv(execInfoEnv, execInfoFor(t, "https://r.ex/k8s/clusters/c-1", nil))
						want = map[string]int{"configload": 1, "stampread": 1}
					}
					var served string
					ops, keyringOps, locks, flocks := count(t, anchorFor(dir), func(anchor *config.CacheAnchor) {
						cmd := tokenCmd{dir: dir, store: store, anchor: anchor}
						in := map[string]string{"server": "https://r.ex", "name": "prod", "cluster": "c-1", "user-id": "u-abc12"}
						if authConfig != "" {
							in["auth-provider"] = authConfig
						}
						res, err := cmd.Execute(t.Context(), tin{s: in}, ui.Discard())
						require.NoError(t, err)
						cred, ok := res.Data.(*execCredential)
						require.True(t, ok)
						served = cred.Status.Token
					})
					assert.Equal(t, "cached-user-tok-"+authConfig, served, "the record of the user id's and auth config's slot is served")
					assert.Equal(t, want, ops, "no read beyond the path's own")
					assert.Zero(t, keyringOps)
					assert.Zero(t, locks)
					assert.Zero(t, flocks)
				})
			}
		}
	})

	t.Run("the plugin's resolved path with no --server, past a change record", func(t *testing.T) {
		// The server comes from the current entry while config.yaml names
		// a store change. Where to connect needs the entry's URL and CA,
		// not its login, so the path reads no credential and no change
		// record before it serves the cache.
		t.Setenv(execInfoEnv, "")
		cdir := t.TempDir()
		const id = "0123456789abcdef0123456789abcdef"
		ccfg := &config.Config{CurrentServer: "prod", CredentialTransitionID: id,
			Servers: map[string]config.Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}
		ccfg.SetPath(config.FilePath(cdir))
		require.NoError(t, ccfg.Write())
		require.NoError(t, os.WriteFile(config.TransitionPath(cdir), []byte("version: 1\nid: "+id+"\nsteps:\n- kind: pin\n  target: file\n"), 0o600))
		cstore := config.NewFileStore(cdir)
		require.NoError(t, seedCache(t, cdir, cstore, key, "cached-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))

		var served string
		ops, keyringOps, locks, flocks := count(t, anchorFor(cdir), func(anchor *config.CacheAnchor) {
			cmd := tokenCmd{dir: cdir, store: cstore, anchor: anchor}
			res, err := cmd.Execute(t.Context(), tin{s: map[string]string{"name": "prod", "cluster": "c-1"}}, ui.Discard())
			require.NoError(t, err)
			cred, ok := res.Data.(*execCredential)
			require.True(t, ok)
			served = cred.Status.Token
		})
		assert.Equal(t, "cached-tok", served, "the record is served")
		assert.Equal(t, map[string]int{"configload": 1, "stampread": 1}, ops,
			"the connection's config load, then the confirmation; no credential read, no change record stat")
		assert.Zero(t, keyringOps)
		assert.Zero(t, locks)
		assert.Zero(t, flocks)
	})

	t.Run("a miss costs no second stamp read", func(t *testing.T) {
		ops, _, locks, flocks := count(t, anchorFor(dir), func(anchor *config.CacheAnchor) {
			cmd := tokenCmd{dir: dir, store: store, anchor: anchor}
			_, _, _, hit := cmd.probeFlagCache("prod", "https://r.ex", "another-cluster", "", "", ui.Discard())
			require.False(t, hit)
		})
		assert.Zero(t, ops["stampread"],
			"the read after the answer exists to confirm a hit, so a miss does not pay it")
		assert.Zero(t, ops["configload"])
		assert.Zero(t, locks)
		assert.Zero(t, flocks, "a key that is not there is not a record to prune")
	})

	t.Run("a stale view costs exactly one reload", func(t *testing.T) {
		// The stamp read, then an unrelated entry change, then the load:
		// the reader's two halves are from different moments.
		anchor := config.NewCacheAnchor(dir)
		mutated, lerr := config.Load(config.FilePath(dir))
		require.NoError(t, lerr)
		mutated.Servers["staging"] = config.Server{URL: "https://s.ex"}
		mutated.SetPath(config.FilePath(dir))
		require.NoError(t, mutated.Write())
		loaded, aerr := config.Load(config.FilePath(dir))
		require.NoError(t, aerr)

		ops, keyringOps, locks, flocks := count(t, anchor.WithConfig(loaded), func(a *config.CacheAnchor) {
			cmd := tokenCmd{dir: dir, store: store, anchor: a}
			_, _, _, hit := cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
			require.True(t, hit, "the record is still this entry's")
		})
		assert.Equal(t, 1, ops["configload"], "exactly one reload, and only on this path")
		assert.Equal(t, 2, ops["stampread"], "the reload's own read, then the confirmation")
		assert.Zero(t, keyringOps, "a reload is not a reason to open the keychain")
		assert.Zero(t, locks)
		assert.Zero(t, flocks, "nor to take the cache flock")
	})

	t.Run("a change landing under the reader costs one reload too", func(t *testing.T) {
		// The other way into the reload: the view was coherent, the
		// record was judged good, and the stamp moved before the
		// confirmation could read it. Three stamp reads in total - the
		// invocation's, the confirmation's, and the one confirming the
		// second judgement - because the reader re-anchors on the value
		// the confirmation read rather than reading the file again.
		late := t.TempDir()
		lcfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}
		lcfg.SetPath(config.FilePath(late))
		require.NoError(t, lcfg.Write())
		lstore := config.NewFileStore(late)
		require.NoError(t, seedCache(t, late, lstore, key, "cached-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))

		anchor := anchorFor(late)

		// The observer fires before each stamp read, so writing from it
		// lands the change between the record read and the confirmation.
		var ops = map[string]int{}
		var mutated, mutating bool
		restore := config.SetIdentityObserverForTest(func(op string) {
			if mutating {
				// The write below is the interleaving, not the reader's
				// own work, so its operations are not the reader's cost.
				return
			}
			ops[op]++
			if op != "stampread" || mutated {
				return
			}
			mutated, mutating = true, true
			defer func() { mutating = false }()
			next, lerr := config.Load(config.FilePath(late))
			require.NoError(t, lerr)
			next.Servers["staging"] = config.Server{URL: "https://s.ex"}
			next.SetPath(config.FilePath(late))
			require.NoError(t, next.Write())
		})
		var locks, flocks int
		restoreLock := config.SetLockHookForTest(func() { locks++ })
		restoreFlock := config.SetWaitForFlockHookForTest(func() { flocks++ })

		cmd := tokenCmd{dir: late, store: lstore, anchor: anchor}
		_, _, _, hit := cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
		restore()
		restoreLock()
		restoreFlock()

		require.True(t, hit, "the record's own entry never moved")
		assert.Equal(t, 1, ops["configload"], "one reload, and only one")
		assert.Equal(t, 2, ops["stampread"],
			"the confirmation that failed, then the one confirming the second judgement")
		assert.Zero(t, locks)
		assert.Zero(t, flocks)
	})

	t.Run("a miss adds only the store check before its write", func(t *testing.T) {
		// A whole plugin process on a miss: the root hook's stamp read and
		// load, resolveDeps' load and lazy store, then the command. The
		// counts are the base's plus the store check at the write: nothing
		// for a declared store, a pin read for `auto`, and with no pin the
		// unpinned decision's look at the identity records and its read of
		// credentials.yaml.
		m := newMadeFrom(t)
		for _, tc := range []struct {
			name, decl, pin string
			want            map[string]int
		}{
			{"a declared store", "file", "",
				map[string]int{"configload": 4, "stampread": 1, "credentialsread": 1}},
			{"auto, pinned", "auto", "file",
				map[string]int{"configload": 4, "stampread": 1, "credentialsread": 1, "pinread": 2}},
			{"auto, unpinned", "auto", "",
				map[string]int{"configload": 4, "stampread": 1, "credentialsread": 3, "pinread": 2, "dirstat": 2}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				mdir := t.TempDir()
				mcfg := &config.Config{CredentialStore: tc.decl, Servers: map[string]config.Server{"prod": {URL: m.URL}}}
				mcfg.SetPath(config.FilePath(mdir))
				require.NoError(t, mcfg.Write())
				if tc.pin != "" {
					require.NoError(t, config.WritePin(mdir, tc.pin))
				}
				require.NoError(t, config.NewFileStore(mdir).SetToken(config.ServerRef{Name: "prod", URL: m.URL}, "alice"))

				ops, keyringOps, locks, flocks := count(t, nil, func(*config.CacheAnchor) {
					anchor := config.NewCacheAnchor(mdir)
					loaded, lerr := config.Load(config.FilePath(mdir))
					require.NoError(t, lerr)
					_, lazy, serr := config.LoadAndStore(mdir, false)
					require.NoError(t, serr)
					cmd := tokenCmd{dir: mdir, store: lazy, anchor: anchor.WithConfig(loaded)}
					_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": m.URL, "name": "prod", "cluster": "local"}}, ui.Discard())
					require.NoError(t, err)
				})
				assert.Equal(t, tc.want, ops)
				assert.Zero(t, keyringOps)
				assert.Zero(t, locks, "an unpaused miss never takes the config lock")
				assert.Equal(t, 1, flocks, "the record write takes the cache flock, once")
				served := tokenCmd{dir: mdir, store: config.NewFileStore(mdir), anchor: anchorFor(mdir)}
				_, _, _, hit := served.probeFlagCache("prod", m.URL, "local", "", "", ui.Discard())
				assert.True(t, hit, "and the record was written")
			})
		}
	})

	t.Run("a miss on a paused entry tries the lock once", func(t *testing.T) {
		// The same whole process on a declared file store, with the
		// entry's cache writes paused: the record is refused before the
		// cache lock, then one try of the config lock and, when taken, one
		// load, one look at the store change record - one stat, since
		// config.yaml names none - and one write that resumes.
		m := newMadeFrom(t)
		base := map[string]int{"configload": 4, "stampread": 1, "credentialsread": 1}
		for _, tc := range []struct {
			name   string
			setup  func(t *testing.T, dir string)
			extra  map[string]int
			locks  int
			writes []string
		}{
			{name: "the lock is free", extra: map[string]int{"configlocktry": 1, "configload": 1, "manifeststat": 1},
				locks: 1, writes: []string{"resume"}},
			{name: "the lock is held", extra: map[string]int{"configlocktry": 1}, setup: func(t *testing.T, dir string) {
				t.Helper()
				other := flock.New(config.LockPath(dir))
				ok, err := other.TryLock()
				require.NoError(t, err)
				require.True(t, ok)
				t.Cleanup(func() { _ = other.Unlock() })
			}},
			{name: "the lock is skipped", setup: func(t *testing.T, _ string) {
				t.Helper()
				t.Setenv(config.EnvConfigNoLock, "1")
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				mdir := pausedPlugin(t, m.URL)
				if tc.setup != nil {
					tc.setup(t, mdir)
				}
				var writes []string
				restoreWrites := config.SetPersistWriteHookForTest(func(step string) error { writes = append(writes, step); return nil })
				defer restoreWrites()
				ops, keyringOps, locks, flocks := count(t, nil, func(*config.CacheAnchor) {
					_, err := kubectl(t, mdir, m.URL, ui.Discard(), nil)
					require.NoError(t, err)
				})
				want := maps.Clone(base)
				for k, v := range tc.extra {
					want[k] += v
				}
				assert.Equal(t, want, ops)
				assert.Zero(t, keyringOps)
				assert.Equal(t, tc.locks, locks)
				assert.Zero(t, flocks, "the refused record never takes the cache flock")
				assert.Equal(t, tc.writes, writes)
			})
		}
	})

	t.Run("pruning a record this directory no longer owns is a write", func(t *testing.T) {
		// The reader paths that take the token-cache flock are both
		// writes: this prune, and the readiness mark a hit pays when the
		// record does not yet name the direct endpoint it was asked for.
		// Neither is the hot path, and neither touches the config lock.
		pruned := t.TempDir()
		pcfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}
		pcfg.SetPath(config.FilePath(pruned))
		require.NoError(t, pcfg.Write())
		pstore := config.NewFileStore(pruned)
		require.NoError(t, seedCache(t, pruned, pstore, key, "cached-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))

		// A login in this directory since the record was written: the
		// entry's binding id moved, so the record is judgeable and invalid.
		relogged, lerr := config.Load(config.FilePath(pruned))
		require.NoError(t, lerr)
		relogged.Servers["prod"] = config.Server{URL: "https://r.ex", CacheBindingID: "b2"}
		relogged.SetPath(config.FilePath(pruned))
		require.NoError(t, relogged.Write())

		ops, keyringOps, locks, flocks := count(t, anchorFor(pruned), func(a *config.CacheAnchor) {
			cmd := tokenCmd{dir: pruned, store: pstore, anchor: a}
			_, _, _, hit := cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
			require.False(t, hit, "a record bound to the previous login is not served")
		})
		assert.Equal(t, 1, flocks, "the prune takes the token-cache flock, once")
		assert.Zero(t, locks, "and never the config lock")
		assert.Zero(t, keyringOps, "a prune is not a reason to open the keychain")
		assert.Zero(t, ops["configload"], "the anchor's config already answered")

		_, gerr := pstore.GetCachedCredDetail(key, anchorFor(pruned))
		require.ErrorIs(t, gerr, config.ErrNoToken, "and the record is gone")
	})
}

// TestTheShimsSlowPathJudgesByTheSameAnchor asserts the anchor reaches
// the shim's slow path, not only its fast-path probe. The slow path
// re-checks the cache under the key it resolved, which can differ from
// the one the probe tried; a command built without an anchor serves
// nothing there, so the shim would re-authenticate on every call with no
// signal.
func TestTheShimsSlowPathJudgesByTheSameAnchor(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(dir)
	key := config.CacheKey("prod", config.CanonicalServerIdentity("https://r.ex"), "c-1")
	require.NoError(t, seedCache(t, dir, store, key, "cached-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))

	t.Run("the resolved anchor is the one the fast path used", func(t *testing.T) {
		anchor := anchorFor(dir)
		assert.Same(t, anchor, shimAnchor(dir, anchor), "an anchor for this directory is kept")
		assert.Nil(t, shimAnchor(t.TempDir(), anchor), "an anchor for another directory answers for nothing")
		assert.Nil(t, shimAnchor(dir, nil), "and with no process anchor there is nothing to judge by")
	})

	t.Run("a command built with it serves the record", func(t *testing.T) {
		cmd := tokenCmd{dir: dir, store: store, anchor: anchorFor(dir)}
		_, hit, err := cmd.cachedResultOnce(t.Context(), key, true, ui.Discard(), nil, "prod", cmd.anchor)
		require.NoError(t, err)
		assert.True(t, hit)
	})

	t.Run("a command built without it serves nothing", func(t *testing.T) {
		cmd := tokenCmd{dir: dir, store: store}
		_, hit, err := cmd.cachedResultOnce(t.Context(), key, true, ui.Discard(), nil, "prod", cmd.anchor)
		require.NoError(t, err)
		assert.False(t, hit, "no anchor means no record can be judged")
	})
}

// TestACrashBetweenTheTwoHalvesTaxesEveryReader asserts the durable cost
// of a config write that did not finish: the stamp file moved and
// config.yaml did not, so no reader's two halves ever line up again.
// Every invocation in that directory pays a reload - one extra config
// load and one extra stamp read - and still serves nothing, on every call,
// until a command writes a stamping config change. The directory keeps
// working, so nothing announces it; the cost is real and is budgeted in
// the budget gate above rather than discovered in production.
func TestACrashBetweenTheTwoHalvesTaxesEveryReader(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(dir)
	key := config.CacheKey("prod", config.CanonicalServerIdentity("https://r.ex"), "c-1")
	require.NoError(t, seedCache(t, dir, store, key, "cached-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))

	// The stamp write landed; the process died before the config write.
	require.NoError(t, os.WriteFile(config.BindingStampPath(dir), []byte("0123456789abcdef0123456789abcdef"), 0o600))

	// The anchor is built outside the measured region, as the root hook
	// builds it: what is counted here is what the reader adds.
	anchor := anchorFor(dir)
	ops := map[string]int{}
	restore := config.SetIdentityObserverForTest(func(op string) { ops[op]++ })
	defer restore()

	cmd := tokenCmd{dir: dir, store: store, anchor: anchor}
	_, _, _, hit := cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
	assert.False(t, hit, "the two halves never line up, so the record is never served")
	assert.Equal(t, 1, ops["configload"], "and the reader pays a reload to find that out")
	assert.Equal(t, 1, ops["stampread"], "re-anchoring reads the stamp again")

	// A stamping config write repairs it, because it writes both halves.
	repaired, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	repaired.Servers["staging"] = config.Server{URL: "https://s.ex"}
	repaired.SetPath(config.FilePath(dir))
	require.NoError(t, repaired.Write())

	anchor = anchorFor(dir)
	ops = map[string]int{}
	cmd = tokenCmd{dir: dir, store: store, anchor: anchor}
	_, _, _, hit = cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
	assert.True(t, hit, "the entry and its binding id never moved, so the record is still good")
	assert.Zero(t, ops["configload"], "and the reload is not paid again")
}

// copyDirectory copies every regular file in src to dst, which is what
// `cp -r` of a config directory does: config.yaml, the binding stamp and
// token-cache.yaml travel together.
func copyDirectory(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	require.NoError(t, err)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(src, e.Name()))
		require.NoError(t, rerr)
		require.NoError(t, os.WriteFile(filepath.Join(dst, e.Name()), b, 0o600))
	}
}

// TestOneRelativeSpellingIsNotOneDirectory asserts the directory a record
// names is the canonical path, not the one the caller typed.
//
// Two people run the CLI with the same `--config .rancher` from two
// working directories. That is two directories, and the CLI resolves the
// flag verbatim, so both processes hold the string `.rancher`. Copy the
// first directory over the second - config.yaml, cachebinding-stamp and
// token-cache.yaml included, which is what copying a config directory
// does - and every other check lines up: the stamp matches, the entry
// exists, its binding id matches. The directory in the record is the only
// thing left that can tell them apart, and `.rancher` on both sides
// cannot: the copy would serve the source's cached cluster credential,
// and after a later login in the source it would be the wrong user's.
//
// Not parallel: it changes the working directory, which is what makes one
// spelling name two places.
func TestOneRelativeSpellingIsNotOneDirectory(t *testing.T) {
	keyring.MockInit()
	const spelling = ".rancher"
	workA, workB := t.TempDir(), t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(workA, spelling), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(workB, spelling), 0o700))

	// Seed the source directory, reached the way its owner reaches it.
	t.Chdir(workA)
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}
	cfg.SetPath(config.FilePath(spelling))
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(spelling)
	key := config.CacheKey("prod", config.CanonicalServerIdentity("https://r.ex"), "c-1")
	require.NoError(t, seedCache(t, spelling, store, key, "source-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))

	cmd := tokenCmd{dir: spelling, store: store, anchor: anchorFor(spelling)}
	_, _, r, hit := cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
	require.True(t, hit, "the directory that made the record serves it")
	require.Equal(t, "source-tok", r.Data.(*execCredential).Status.Token)

	copyDirectory(t, filepath.Join(workA, spelling), filepath.Join(workB, spelling))

	// The same spelling, a different place.
	t.Chdir(workB)
	copied, lerr := config.Load(config.FilePath(spelling))
	require.NoError(t, lerr)
	require.Equal(t, cfg.BindingStamp, copied.BindingStamp, "the stamp travelled with the copy")
	require.Equal(t, "b1", copied.Servers["prod"].CacheBindingID, "and so did the entry's binding")

	storeB := config.NewFileStore(spelling)
	t.Run("the native plugin serves nothing in the copy", func(t *testing.T) {
		cmd := tokenCmd{dir: spelling, store: storeB, anchor: anchorFor(spelling)}
		_, _, _, hit := cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
		assert.False(t, hit)
	})

	t.Run("the legacy shim serves nothing in the copy", func(t *testing.T) {
		_, _, _, hit, _ := shimProbeFlagCache(spelling, "prod", "https://r.ex", "c-1", anchorFor(spelling), ui.Discard())
		assert.False(t, hit)
	})

	t.Run("a read-only reader serves nothing in the copy", func(t *testing.T) {
		// The shape live completion takes: the same validation over the
		// same records, through the read-only helper.
		entries, err := config.ReadOnlyCachedCredsForClusterWithTokens(
			spelling, config.CanonicalServerIdentity("https://r.ex"), "c-1", anchorFor(spelling))
		require.NoError(t, err)
		assert.Empty(t, entries)
	})
}

// TestOneDirectoryReachedByTwoSpellingsIsOneDirectory asserts the other
// half of the same rule: `./x`, `x/../x` and a trailing separator all
// name one directory, and a record written under one spelling is served
// under the others. Canonicalizing is what makes both halves true at
// once.
func TestOneDirectoryReachedByTwoSpellingsIsOneDirectory(t *testing.T) {
	keyring.MockInit()
	base := t.TempDir()
	dir := filepath.Join(base, "x")
	require.NoError(t, os.Mkdir(dir, 0o700))

	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	store := config.NewFileStore(dir)
	key := config.CacheKey("prod", config.CanonicalServerIdentity("https://r.ex"), "c-1")
	require.NoError(t, seedCache(t, dir, store, key, "tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))

	for _, spelling := range []string{
		dir,
		filepath.Join(base, ".", "x"),
		filepath.Join(base, "x", "..", "x"),
		dir + string(filepath.Separator),
	} {
		cmd := tokenCmd{dir: spelling, store: config.NewFileStore(spelling), anchor: anchorFor(spelling)}
		_, _, r, hit := cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
		require.True(t, hit, "spelling %q names the same directory", spelling)
		assert.Equal(t, "tok", r.Data.(*execCredential).Status.Token)
	}
}

// TestAnUnrelatedEntryChangeCostsOneReloadNotTheHit asserts the reload
// through a real reader, which is the only place it means anything.
//
// The interleaving: the root hook reads the binding stamp, and before its
// config load lands, an unrelated entry is added - a `config add-server`
// in another process, say. That write advances the directory's stamp, so
// the reader's own two halves are from different moments. Nothing about
// the record's entry has changed; its binding id is what it always was.
//
// Treating that as a miss would send the exec plugin off to derive a
// fresh cluster credential every time anything else in the directory
// moved. The reader re-anchors on the newer stamp, loads once more, and
// serves the record.
func TestAnUnrelatedEntryChangeCostsOneReloadNotTheHit(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	store := config.NewFileStore(dir)
	key := config.CacheKey("prod", config.CanonicalServerIdentity("https://r.ex"), "c-1")
	require.NoError(t, seedCache(t, dir, store, key, "cached-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))

	// The stamp read happens first, as the root hook does it.
	anchor := config.NewCacheAnchor(dir)

	// Then an unrelated entry arrives, which stamps the directory.
	mutated, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	mutated.Servers["staging"] = config.Server{URL: "https://s.ex"}
	mutated.SetPath(config.FilePath(dir))
	require.NoError(t, mutated.Write())

	// The config load returns the mutated file, so the reader holds a
	// stamp from before it and a config from after.
	loaded, aerr := config.Load(config.FilePath(dir))
	require.NoError(t, aerr)
	require.NotEqual(t, anchor.Stamp, loaded.BindingStamp, "the write landed between the two")
	require.Equal(t, "b1", loaded.Servers["prod"].CacheBindingID, "and left this entry alone")

	cmd := tokenCmd{dir: dir, store: store, anchor: anchor.WithConfig(loaded)}
	_, _, r, hit := cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
	require.True(t, hit, "an unrelated entry change does not cost the hit")
	assert.Equal(t, "cached-tok", r.Data.(*execCredential).Status.Token)
}

// TestAChangeToTheEntryItselfIsStillAMiss asserts the reload does not
// paper over the case the stamp exists for. Same interleaving, except the
// write that landed is a login for this record's own entry: the reload
// finds the entry's binding advanced and the record is a miss, which is
// the whole point.
func TestAChangeToTheEntryItselfIsStillAMiss(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	store := config.NewFileStore(dir)
	key := config.CacheKey("prod", config.CanonicalServerIdentity("https://r.ex"), "c-1")
	require.NoError(t, seedCache(t, dir, store, key, "previous-users-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))

	anchor := config.NewCacheAnchor(dir)

	// A login for this entry completes: its binding advances.
	mutated, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	entry := mutated.Servers["prod"]
	require.NoError(t, config.AdvanceCacheBinding(&entry))
	mutated.Servers["prod"] = entry
	mutated.SetPath(config.FilePath(dir))
	require.NoError(t, mutated.Write())

	loaded, aerr := config.Load(config.FilePath(dir))
	require.NoError(t, aerr)

	cmd := tokenCmd{dir: dir, store: store, anchor: anchor.WithConfig(loaded)}
	_, _, _, hit := cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
	assert.False(t, hit, "the previous login's credential is never served after a completed one")
}
