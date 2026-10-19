package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stampOnDisk reads the stamp file directly, so a test can tell what the
// directory holds from what a Config happens to carry.
func stampOnDisk(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(BindingStampPath(dir))
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(t, err)
	return strings.TrimRight(string(b), "\n")
}

// seedStampedConfig writes a config with one entry and returns the
// directory and the stamp that write produced.
func seedStampedConfig(t *testing.T) (dir string, cfg *Config, stamp string) {
	t.Helper()
	dir = t.TempDir()
	cfg = &Config{
		CurrentServer: "prod",
		Servers:       map[string]Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}},
	}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	reloaded, err := Load(FilePath(dir))
	require.NoError(t, err)
	return dir, reloaded, stampOnDisk(t, dir)
}

// TestConfigWriteStampsWhatCanInvalidateARecord asserts the rule the
// binding stamp is written by: a write that changes which entries exist,
// which URL one answers for, which cache binding id one carries, which
// owner one records, the current server or the token-reuse setting
// advances the stamp, and a write that changes anything else does not. A
// cache record's validity depends on exactly those six, so a write that
// touches none of them cannot invalidate one, and stamping it would throw
// away every warm hit in the directory for nothing.
func TestConfigWriteStampsWhatCanInvalidateARecord(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Config)
		stamped bool
	}{
		{name: "an entry is added", stamped: true, mutate: func(c *Config) {
			c.Servers["staging"] = Server{URL: "https://s.ex"}
		}},
		{name: "an entry is removed", stamped: true, mutate: func(c *Config) {
			delete(c.Servers, "prod")
		}},
		{name: "an entry is re-addressed", stamped: true, mutate: func(c *Config) {
			e := c.Servers["prod"]
			e.URL = "https://moved.ex"
			c.Servers["prod"] = e
		}},
		{name: "a cache binding id advances", stamped: true, mutate: func(c *Config) {
			e := c.Servers["prod"]
			e.CacheBindingID = "b2"
			c.Servers["prod"] = e
		}},
		{name: "an entry's recorded owner changes", stamped: true, mutate: func(c *Config) {
			e := c.Servers["prod"]
			e.TokenUserID = "u-abc12"
			c.Servers["prod"] = e
		}},
		{name: "an entry's recorded owner is written with its own value", mutate: func(c *Config) {
			e := c.Servers["prod"]
			e.TokenUserID = ""
			c.Servers["prod"] = e
		}},
		{name: "the current server pointer moves", stamped: true, mutate: func(c *Config) {
			c.CurrentServer = ""
		}},
		{name: "the current server pointer is written with its own value", mutate: func(c *Config) {
			c.CurrentServer = "prod"
		}},
		{name: "a completion preference changes", mutate: func(c *Config) {
			c.Completion.Network = true
		}},
		{name: "token reuse is turned on", stamped: true, mutate: func(c *Config) {
			c.TokenReuse = true
		}},
		{name: "token reuse is written with its own value", mutate: func(c *Config) {
			c.TokenReuse = false
		}},
		{name: "an entry's trust changes", mutate: func(c *Config) {
			e := c.Servers["prod"]
			e.CA, e.CAFingerprint = "pem", "f0"
			c.Servers["prod"] = e
		}},
		{name: "nothing changes", mutate: func(*Config) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, cfg, before := seedStampedConfig(t)
			require.NotEmpty(t, before)

			tc.mutate(cfg)
			require.NoError(t, cfg.Write())

			after := stampOnDisk(t, dir)
			reloaded, err := Load(FilePath(dir))
			require.NoError(t, err)
			assert.Equal(t, after, reloaded.BindingStamp, "the file and the config always carry the same value")
			if tc.stamped {
				assert.NotEqual(t, before, after, "a write that can invalidate a record advances the stamp")
				return
			}
			assert.Equal(t, before, after, "a write that cannot invalidate a record leaves the stamp alone")
		})
	}
}

// TestConfigWriteStampsTheFileBeforeTheConfig asserts the order the two
// writes happen in. A crash between them must leave the directory failing
// every cache hit closed, which needs the file to be ahead of the config;
// the other order would leave records from before the change validating
// against a config that already moved on.
func TestConfigWriteStampsTheFileBeforeTheConfig(t *testing.T) {
	dir, cfg, before := seedStampedConfig(t)
	// A directory where config.yaml belongs makes the config write fail
	// and leaves the stamp write, which runs first, untouched.
	require.NoError(t, os.Remove(FilePath(dir)))
	require.NoError(t, os.Mkdir(FilePath(dir), 0o700))

	cfg.Servers["staging"] = Server{URL: "https://s.ex"}
	require.Error(t, cfg.Write())

	after := stampOnDisk(t, dir)
	assert.NotEqual(t, before, after, "the stamp file is written first and is not put back")
}

// TestBindingStampReadStates asserts what a reader makes of the file. An
// absent one is the bare-install state and reads as the empty stamp, which
// matches a config that carries none, so the exec plugin still serves its
// cache on a directory nothing has written. Anything that is not a stamp
// reads as a value no config can carry, so every hit in the directory
// misses - and the read itself never fails, because it happens at the
// start of every command and an error would take the whole directory
// down, the login and logout that would repair it included.
func TestBindingStampReadStates(t *testing.T) {
	t.Run("absent reads as empty and usable", func(t *testing.T) {
		stamp, ok := readBindingStamp(t.TempDir())
		assert.Empty(t, stamp)
		assert.True(t, ok, "a directory that never wrote one is not damaged")
	})

	t.Run("a written stamp round-trips", func(t *testing.T) {
		dir := t.TempDir()
		want, err := newBindingStamp()
		require.NoError(t, err)
		require.NoError(t, writeBindingStamp(dir, want))
		got, _ := readBindingStamp(dir)
		assert.Equal(t, want, got)
	})

	for _, tc := range []struct {
		name, content string
		asDir         bool
	}{
		{name: "empty", content: ""},
		{name: "not hex", content: "not-a-stamp\n"},
		{name: "too short", content: "abcd\n"},
		{name: "upper case", content: "ABCDEF0123456789ABCDEF0123456789\n"},
		{name: "not a regular file", asDir: true},
	} {
		t.Run("malformed: "+tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.asDir {
				require.NoError(t, os.Mkdir(BindingStampPath(dir), 0o700))
			} else {
				require.NoError(t, os.WriteFile(BindingStampPath(dir), []byte(tc.content), 0o600))
			}
			got, ok := readBindingStamp(dir)
			assert.False(t, ok, "the file cannot be used")
			assert.Empty(t, got)

			// The damage is carried beside the value, not in it, so no
			// config.yaml content can reproduce it and compare equal.
			anchor := NewCacheAnchor(dir)
			for _, cfg := range []*Config{
				{},
				{BindingStamp: "unreadable"},
				{BindingStamp: "damaged"},
			} {
				assert.False(t, anchor.WithConfig(cfg).judgeable(),
					"a damaged stamp costs the directory its warm cache whatever the config says")
			}
			assert.True(t, anchor.Damaged())

			// And the way out runs: a write replaces the file rather
			// than failing on it.
			require.NoError(t, writeBindingStamp(dir, mustStamp(t)))
			fresh, fok := readBindingStamp(dir)
			require.True(t, fok)
			assert.NotEmpty(t, fresh)
		})
	}
}

// advanceBothHalves completes a binding change the way a config write
// does: the stamp file first, then config.yaml carrying the same value,
// so a reader that reloads afterwards finds a coherent view.
func advanceBothHalves(t *testing.T, dir string) {
	t.Helper()
	next, err := newBindingStamp()
	require.NoError(t, err)
	require.NoError(t, writeBindingStamp(dir, next))
	cfg, lerr := Load(FilePath(dir))
	require.NoError(t, lerr)
	cfg.BindingStamp = next
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
}

// mustStamp is a fresh stamp for a test that only needs a valid one.
func mustStamp(t *testing.T) string {
	t.Helper()
	s, err := newBindingStamp()
	require.NoError(t, err)
	return s
}

// TestBindingStampsAreDistinctBetweenWriters asserts the stamp is a fresh
// random value rather than a counter. Two writers that escaped the flock
// under RANCHER_CLI_CONFIG_NO_LOCK both write, and the point is that
// neither can produce the value the other did: a counter would have them
// both write the same next number and leave a reader satisfied with stale
// entries.
func TestBindingStampsAreDistinctBetweenWriters(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		stamp, err := newBindingStamp()
		require.NoError(t, err)
		require.Len(t, stamp, randomIDLen)
		assert.False(t, seen[stamp], "a stamp is never reused")
		seen[stamp] = true
	}
}

// TestBindingStampReadIsCounted asserts the read reports itself to the
// operation observer, so the hot-path budget - one stamp read per command
// and one more per cache hit - can be asserted rather than assumed.
func TestBindingStampReadIsCounted(t *testing.T) {
	var ops []string
	restore := SetIdentityObserverForTest(func(op string) { ops = append(ops, op) })
	defer restore()

	readBindingStamp(t.TempDir())
	assert.Equal(t, []string{"stampread"}, ops)
}

// TestBindingStampPathIsInTheDirectory asserts the stamp is directory
// state like the pin, not something a portable config.yaml carries alone.
func TestBindingStampPathIsInTheDirectory(t *testing.T) {
	t.Parallel()
	assert.Equal(t, filepath.Join("/tmp/x", "cachebinding-stamp"), BindingStampPath("/tmp/x"))
}

// TestCanonicalDirResolvesEverySpellingOfOneDirectory asserts the value a
// record is bound to: the absolute, cleaned path, so any spelling that
// names one directory produces one value, and a spelling that cannot be
// resolved produces a value that matches nothing rather than one that
// might match another process's.
func TestCanonicalDirResolvesEverySpellingOfOneDirectory(t *testing.T) {
	dir := t.TempDir()
	canon := CanonicalDir(dir)
	require.True(t, filepath.IsAbs(canon), "the bound value is always absolute")

	for _, spelling := range []string{
		dir,
		dir + string(filepath.Separator),
		filepath.Join(dir, "."),
		filepath.Join(dir, "sub", ".."),
	} {
		assert.Equal(t, canon, CanonicalDir(spelling), "spelling %q names the same directory", spelling)
	}

	t.Run("a relative spelling is resolved against the working directory", func(t *testing.T) {
		parent := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(parent, "cfg"), 0o700))
		t.Chdir(parent)
		assert.Equal(t, CanonicalDir(filepath.Join(parent, "cfg")), CanonicalDir("cfg"))
	})

	t.Run("an unresolvable directory matches nothing", func(t *testing.T) {
		// CanonicalDir yields the empty value when the path cannot be
		// made absolute. Both halves of the judgement refuse it, so two
		// processes that both failed to resolve never match each other.
		// Everything but the directory lines up, so the directory is
		// what each of these turns on.
		rec := func(recDir string) cachedCred {
			return cachedCred{Version: cacheRecordVersion, Dir: recDir, Name: "prod", Server: "https://r.ex",
				Kind: recordOwnCredential, Identified: "prod", Entry: "prod", BindingID: "b1"}
		}
		cfg := &Config{Servers: map[string]Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}
		require.True(t, (&CacheAnchor{Dir: canon, Cfg: cfg}).valid(rec(canon)), "the control case is a hit")

		unresolvable := &CacheAnchor{Dir: "", Cfg: cfg}
		assert.False(t, unresolvable.valid(rec("")), "two unresolved directories are not one directory")
		assert.False(t, unresolvable.SameDirectory(dir))
		assert.False(t, (&CacheAnchor{Dir: canon, Cfg: cfg}).valid(rec("")))
	})
}

// testCacheBinding binds a record to dir and no entry. Tests whose subject
// is something other than the binding - deletion by server URL, expiry
// pruning, readiness marking - use it so their records stay readable
// without having to stand up a login.
func testCacheBinding(dir string) CacheBinding { return CacheBindingForTest(CanonicalDir(dir), "", "") }

// testAnchor is the anchor a reader in dir holds: the stamp read first,
// the config loaded after it. A directory with no entries serves a record
// bound to no entry, which is what testCacheBinding writes.
func testAnchor(t *testing.T, dir string) *CacheAnchor {
	t.Helper()
	anchor := NewCacheAnchor(dir)
	cfg, lerr := Load(FilePath(dir))
	require.NoError(t, lerr)
	return anchor.WithConfig(cfg)
}

// TestCacheRecordValidity asserts what makes a record servable. The
// binding is what says who a cached cluster credential belongs to: the
// directory that created it, and what the exec plugin's lookup for its
// call read - here, a record made from the identified entry's own
// credential, bound to that entry's cache binding id, and one of a call
// that identified no entry, bound exactly to the server's entries. Any of
// them disagreeing means the credential is not this reader's to serve.
func TestCacheRecordValidity(t *testing.T) {
	const dir = "/tmp/dir"
	cfg := func(stamp string, servers map[string]Server) *Config {
		return &Config{BindingStamp: stamp, Servers: servers}
	}
	bound := map[string]Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}
	own := func(change func(*cachedCred)) cachedCred {
		rec := cachedCred{Version: cacheRecordVersion, Dir: dir, Name: "prod", Server: "https://r.ex",
			Kind: recordOwnCredential, Identified: "prod", Entry: "prod", BindingID: "b1"}
		if change != nil {
			change(&rec)
		}
		return rec
	}
	unnamed := func(server string) cachedCred {
		none, off := "", false
		return cachedCred{Version: cacheRecordVersion, Dir: dir, Name: "kube", Server: server, Kind: recordServer,
			Entries: &[]cachedEntry{}, CurrentServer: &none, TokenReuse: &off}
	}

	for _, tc := range []struct {
		name  string
		rec   cachedCred
		a     *CacheAnchor
		valid bool
	}{
		{
			name:  "the entry and its binding agree",
			rec:   own(nil),
			a:     &CacheAnchor{Dir: dir, Stamp: "s1", Cfg: cfg("s1", bound)},
			valid: true,
		},
		{
			name: "the entry's binding has advanced",
			rec:  own(func(r *cachedCred) { r.BindingID = "b0" }),
			a:    &CacheAnchor{Dir: dir, Stamp: "s1", Cfg: cfg("s1", bound)},
		},
		{
			name: "the entry is gone",
			rec:  own(nil),
			a:    &CacheAnchor{Dir: dir, Stamp: "s1", Cfg: cfg("s1", nil)},
		},
		{
			name: "another directory created it",
			rec:  own(func(r *cachedCred) { r.Dir = "/tmp/other" }),
			a:    &CacheAnchor{Dir: dir, Stamp: "s1", Cfg: cfg("s1", bound)},
		},
		{
			name: "it names no directory at all",
			rec:  own(func(r *cachedCred) { r.Dir = "" }),
			a:    &CacheAnchor{Dir: dir, Stamp: "s1", Cfg: cfg("s1", bound)},
		},
		{
			name: "a record written before the binding fields existed",
			rec:  own(func(r *cachedCred) { r.Version = 0 }),
			a:    &CacheAnchor{Dir: dir, Stamp: "s1", Cfg: cfg("s1", bound)},
		},
		{
			name: "a record written in the shape before this one",
			rec:  cachedCred{Version: 1, Dir: dir, Name: "prod", Server: "https://r.ex", Entry: "prod", BindingID: "b1"},
			a:    &CacheAnchor{Dir: dir, Stamp: "s1", Cfg: cfg("s1", bound)},
		},
		{
			name: "the load was overtaken by a binding change",
			rec:  own(nil),
			a:    &CacheAnchor{Dir: dir, Stamp: "s0", Cfg: cfg("s1", bound)},
		},
		{
			name: "the config could not be read",
			rec:  own(nil),
			a:    &CacheAnchor{Dir: dir, Stamp: "s1"},
		},
		{
			name: "no anchor at all",
			rec:  own(nil),
		},
		{
			name: "an entry whose login predates the binding id",
			rec:  own(func(r *cachedCred) { r.BindingID = "" }),
			a: &CacheAnchor{Dir: dir, Stamp: "s1", Cfg: cfg("s1",
				map[string]Server{"prod": {URL: "https://r.ex"}})},
			valid: true,
		},
		{
			name:  "no entry, and the server has none either",
			rec:   unnamed("https://r.ex"),
			a:     &CacheAnchor{Dir: dir, Stamp: "s1", Cfg: cfg("s1", nil)},
			valid: true,
		},
		{
			name: "no entry, but the server has one now",
			rec:  unnamed("https://r.ex"),
			a:    &CacheAnchor{Dir: dir, Stamp: "s1", Cfg: cfg("s1", bound)},
		},
		{
			name: "no entry, and another server's entry exists",
			rec:  unnamed("https://other.ex"),
			a:    &CacheAnchor{Dir: dir, Stamp: "s1", Cfg: cfg("s1", bound)},
			// The entry that exists answers for a different server, so
			// this record's "the directory had no entry" still holds.
			valid: true,
		},
		{
			name:  "the bare install: no stamp anywhere, no entries",
			rec:   unnamed("https://r.ex"),
			a:     &CacheAnchor{Dir: dir, Cfg: cfg("", nil)},
			valid: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.valid, tc.a.valid(tc.rec))
			if !tc.valid && tc.a.judgeable() {
				assert.True(t, tc.a.staleRecord("k", tc.rec), "a record judged not ours is cleaned up")
			}
			if !tc.a.judgeable() {
				assert.False(t, tc.a.staleRecord("k", tc.rec), "a reader that cannot judge deletes nothing")
			}
		})
	}
}

// TestCachedHitBoundsItsRetry asserts the shape of the protocol around
// one reload, in the cases a concrete reader cannot easily be driven
// into: a view that was already out of date before anything was read, a
// directory being rewritten under the reader, and a record that was
// judged and found wanting.
//
// The interleaving this exists for is covered through a real reader in
// internal/token (an unrelated entry mutation landing between the stamp
// read and the config load); what is here is the bound and the three
// outcomes.
func TestCachedHitBoundsItsRetry(t *testing.T) {
	seed := func(t *testing.T) (dir string, cfg *Config) {
		t.Helper()
		dir = t.TempDir()
		cfg = &Config{Servers: map[string]Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}
		cfg.SetPath(FilePath(dir))
		require.NoError(t, cfg.Write())
		return dir, cfg
	}

	t.Run("a view that was already out of date reads nothing, then reads once", func(t *testing.T) {
		dir, cfg := seed(t)
		// The stamp read happened before a write that the config load
		// then picked up: the reader's two halves are from different
		// moments before it has looked at any record.
		stale := &CacheAnchor{Dir: dir, Stamp: "0000000000000000000000000000dead", Cfg: cfg}
		require.True(t, stale.staleView())

		var reads int
		got, hit, err := CachedHit(stale, func(a *CacheAnchor) (string, bool, error) {
			reads++
			assert.False(t, a.staleView(), "the reader is only ever handed a view that holds together")
			return "tok", true, nil
		})
		require.NoError(t, err)
		assert.True(t, hit, "the reload found a config and a stamp that agree")
		assert.Equal(t, "tok", got)
		assert.Equal(t, 1, reads, "nothing is judged against entries known to be out of date")
	})

	t.Run("a directory being rewritten under the reader is a miss", func(t *testing.T) {
		dir, _ := seed(t)
		var reads int
		_, hit, err := CachedHit(testAnchor(t, dir), func(a *CacheAnchor) (string, bool, error) {
			reads++
			// Something completes a binding change between every read,
			// writing both halves, so each reload finds a view that
			// holds together and each confirmation still fails.
			advanceBothHalves(t, dir)
			return "tok", true, nil
		})
		require.NoError(t, err)
		assert.False(t, hit)
		assert.Equal(t, 2, reads, "one reload, never a loop")
	})

	t.Run("a record judged and found wanting is never revalidated", func(t *testing.T) {
		dir, _ := seed(t)
		var reads int
		_, hit, err := CachedHit(testAnchor(t, dir), func(a *CacheAnchor) (string, bool, error) {
			reads++
			return "", false, nil
		})
		require.NoError(t, err)
		assert.False(t, hit)
		assert.Equal(t, 1, reads, "looking again cannot change a verdict the reader already reached")
	})
}

// TestCacheHitReadsTheStampTwice asserts the hot-path budget: one stamp
// read per command, taken before the config load, and one more before the
// credential is handed back. Anything else is a cost paid on every
// kubectl call and multiplied by the harness's fan-out.
func TestCacheHitReadsTheStampTwice(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{Servers: map[string]Server{"prod": {URL: "https://r.ex", CacheBindingID: "b1"}}}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())

	var ops []string
	restore := SetIdentityObserverForTest(func(op string) { ops = append(ops, op) })
	defer restore()

	anchor := testAnchor(t, dir)
	require.Equal(t, []string{"stampread", "configload"}, ops,
		"the invocation's own pair: the stamp read first, then the load it anchors")

	_, hit, err := CachedHit(anchor, func(*CacheAnchor) (string, bool, error) { return "tok", true, nil })
	require.NoError(t, err)
	require.True(t, hit)
	assert.Equal(t, []string{"stampread", "configload", "stampread"}, ops,
		"a hit adds one stamp read before the credential is returned, and no load")
}

// TestAConfigWriteReplacesANonRegularStampPath asserts the way out of a
// damaged stamp file actually runs. The read side deliberately turns a
// damaged file into a cold cache rather than an error, and that is only
// true if the login or logout it points at can rewrite the file: an
// atomic rename cannot replace a directory, so without this every config
// write in the directory would fail with a raw rename error naming
// neither the cause nor the fix.
func TestAConfigWriteReplacesANonRegularStampPath(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())

	require.NoError(t, os.RemoveAll(BindingStampPath(dir)))
	require.NoError(t, os.Mkdir(BindingStampPath(dir), 0o700))
	_, ok := readBindingStamp(dir)
	require.False(t, ok, "the directory starts damaged")

	// A write that changes the entry lookup has to stamp, which is the
	// write that has to get past the bad path.
	reloaded, lerr := Load(FilePath(dir))
	require.NoError(t, lerr)
	reloaded.Servers["prod"] = Server{URL: "https://r.ex"}
	require.NoError(t, reloaded.Write(), "the login that repairs the directory is not blocked by it")

	stamp, sok := readBindingStamp(dir)
	require.True(t, sok)
	assert.Equal(t, reloaded.BindingStamp, stamp, "and the two halves agree again")
}
