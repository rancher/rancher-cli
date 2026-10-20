package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// authConfigDirectory is a directory with entries of alice for one URL that
// recorded different auth configs: prod (corp-ldap), staging (none), other
// (openldap), her plugin login entries one per case, and bob's entry, which
// recorded corp-ldap.
func authConfigDirectory() *Config {
	plugin := func(id, authConfig string) Server {
		return Server{URL: "https://r.ex", CacheBindingID: id, TokenUserID: "u-alice", TokenAuthConfig: authConfig, Provenance: clierr.ProvenanceExecLogin}
	}
	return &Config{CurrentServer: "prod", TokenReuse: true, Servers: map[string]Server{
		"prod":               {URL: "https://r.ex", CacheBindingID: "b-prod", TokenUserID: "u-alice", TokenAuthConfig: "corp-ldap"},
		"staging":            {URL: "https://r.ex", CacheBindingID: "b-staging", TokenUserID: "u-alice"},
		"other":              {URL: "https://r.ex", CacheBindingID: "b-other", TokenUserID: "u-alice", TokenAuthConfig: "openldap"},
		"bobs":               {URL: "https://r.ex", CacheBindingID: "b-bob", TokenUserID: "u-bob", TokenAuthConfig: "corp-ldap"},
		"exec-login-r.ex-a":  plugin("b-pa", "corp-ldap"),
		"exec-login-r.ex-b":  plugin("b-pb", ""),
		"exec-login-r.ex-c":  plugin("b-pc", "openldap"),
		"exec-login-r.ex-d":  plugin("b-pd", "corp-ldap"),
		"exec-login-r.ex-e":  plugin("b-pe", ""),
		"elsewhere-corpldap": {URL: "https://other.ex", CacheBindingID: "b-else", TokenUserID: "u-alice", TokenAuthConfig: "corp-ldap"},
	}}
}

// TestCacheKeyForCarriesTheAuthConfigAfterTheUserID asserts the key of a
// call with an auth config puts it after the user id and before the name,
// and that the three forms - without --user-id, with it, with it and an
// auth config - never meet, whatever --name is: the user id and the auth
// config are object names, which hold neither ":" nor "/", so no --name that
// the reserved prefix leaves free can produce either form. An auth config
// without a user id is not part of a key.
func TestCacheKeyForCarriesTheAuthConfigAfterTheUserID(t *testing.T) {
	t.Parallel()
	const srv, cluster = "https://r.ex", "c-1"
	assert.Equal(t, "user-id:u-abc12:corp-ldap/prod-c-1@https://r.ex/c-1", CacheKeyFor("prod-c-1", srv, cluster, "u-abc12", "corp-ldap"))
	assert.Equal(t, "user-id:u-abc12:corp-ldap/@https://r.ex/c-1", CacheKeyFor("", srv, cluster, "u-abc12", "corp-ldap"), "no --name leaves the name part empty")
	assert.Equal(t, "user-id:u-abc12/prod-c-1@https://r.ex/c-1", CacheKeyFor("prod-c-1", srv, cluster, "u-abc12", ""), "without an auth config the key is the user's")
	assert.Equal(t, "prod-c-1@https://r.ex/c-1", CacheKeyFor("prod-c-1", srv, cluster, "", "corp-ldap"), "an auth config without a user id is not in the key")
	assert.NotEqual(t, CacheKeyFor("n", srv, cluster, "u-a", "corp-ldap"), CacheKeyFor("n", srv, cluster, "u-a", "openldap"))
	assert.NotEqual(t, CacheKeyFor("n", srv, cluster, "u-a", "corp-ldap"), CacheKeyFor("n", srv, cluster, "u-a", ""))
	// A name that looks like the rest of the key cannot reach it: the key of
	// a call with a user id begins with the reserved prefix, which no --name
	// may, and a user id and an auth config hold no ":" or "/".
	assert.NotEqual(t, CacheKeyFor("corp-ldap/prod", srv, cluster, "u-a", ""), CacheKeyFor("prod", srv, cluster, "u-a", "corp-ldap"))
	assert.NotEqual(t, CacheKeyFor("b:corp-ldap/prod", srv, cluster, "u-a", ""), CacheKeyFor("prod", srv, cluster, "u-a", "corp-ldap"))
}

// TestTheUserIDAndTheAuthConfigOfAKeyEndAtTheirSeparators asserts the two
// readers of a key: the user id ends at the first ":" or "/", the auth
// config is what lies between that ":" and the next "/", and a name or a
// host with a ":" after the first "/" never moves either.
func TestTheUserIDAndTheAuthConfigOfAKeyEndAtTheirSeparators(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		key, user, authConfig string
	}{
		{"user-id:u-abc12:corp-ldap/prod@https://r.ex/c-1", "u-abc12", "corp-ldap"},
		{"user-id:u-abc12/prod@https://r.ex/c-1", "u-abc12", ""},
		{"user-id:u-abc12/prod:corp-ldap@https://r.ex:8443/c-1", "u-abc12", ""},
		{"user-id:u-abc12:corp-ldap/pr:od@https://r.ex:8443/c-1", "u-abc12", "corp-ldap"},
		{"user-id:u-abc12:corp-ldap", "u-abc12", "corp-ldap"},
		{"user-id:u-abc12", "u-abc12", ""},
		{"prod@https://r.ex/c-1", "", ""},
		{"prod:x@https://r.ex/c-1", "", ""},
	} {
		assert.Equal(t, tc.user, userIDOfKey(tc.key), tc.key)
		assert.Equal(t, tc.authConfig, authConfigOfKey(tc.key), tc.key)
	}
}

// TestEntryListsKeepTheEntriesOfTheAuthConfigAndTheUnrecordedOnes asserts
// the two lists of a call with an auth config - the entries that record the
// user (OwnedServerNames) and, among them, the plugin login entries
// (PluginLoginEntriesOf) - leave out an entry that recorded another auth
// config and keep, in name order, the ones that recorded the call's and the
// ones that recorded none, whether or not they hold a credential; without
// an auth config they are the user's alone.
func TestEntryListsKeepTheEntriesOfTheAuthConfigAndTheUnrecordedOnes(t *testing.T) {
	t.Parallel()
	cfg := authConfigDirectory()
	const srv = "https://r.ex"
	assert.Equal(t, []string{"exec-login-r.ex-a", "exec-login-r.ex-b", "exec-login-r.ex-d", "exec-login-r.ex-e", "prod", "staging"},
		cfg.OwnedServerNames(srv, "u-alice", "corp-ldap"), "another auth config's entries are out, the unrecorded ones stay")
	assert.Equal(t, []string{"exec-login-r.ex-a", "exec-login-r.ex-b", "exec-login-r.ex-c", "exec-login-r.ex-d", "exec-login-r.ex-e", "other", "prod", "staging"},
		cfg.OwnedServerNames(srv, "u-alice", ""), "without an auth config every entry of the user is listed")
	assert.Empty(t, cfg.OwnedServerNames(srv, "u-nobody", "corp-ldap"))
	assert.Equal(t, []string{"exec-login-r.ex-a", "exec-login-r.ex-b", "exec-login-r.ex-d", "exec-login-r.ex-e"},
		cfg.PluginLoginEntriesOf(srv, "u-alice", "corp-ldap"))
	assert.Equal(t, []string{"exec-login-r.ex-a", "exec-login-r.ex-b", "exec-login-r.ex-c", "exec-login-r.ex-d", "exec-login-r.ex-e"},
		cfg.PluginLoginEntriesOf(srv, "u-alice", ""))
	assert.Equal(t, []string{"exec-login-r.ex-a", "exec-login-r.ex-b", "exec-login-r.ex-c", "exec-login-r.ex-d", "exec-login-r.ex-e"},
		cfg.PluginLoginEntriesOf(srv, "", "corp-ldap"), "without a user the plugin login entries are all of them")
}

// TestTheAnsweringPluginEntriesAreTheOnesOfTheAuthConfigElseTheUnrecorded
// asserts the group that answers as the identified entry's fallback: the
// plugin login entries that recorded the call's auth config when there are
// any, else the ones that recorded none, and the three places that count
// them - the call's lookup, a cache hit and the binding's refusal - agree.
func TestTheAnsweringPluginEntriesAreTheOnesOfTheAuthConfigElseTheUnrecorded(t *testing.T) {
	t.Parallel()
	const srv = "https://r.ex"
	cfg := authConfigDirectory()
	assert.Equal(t, []string{"exec-login-r.ex-a", "exec-login-r.ex-d"}, cfg.AnsweringPluginEntries(srv, "u-alice", "corp-ldap"),
		"the entries of the auth config answer, the unrecorded ones do not count")
	assert.Equal(t, []string{"exec-login-r.ex-c"}, cfg.AnsweringPluginEntries(srv, "u-alice", "openldap"))

	for name := range cfg.Servers {
		if s := cfg.Servers[name]; s.Provenance == clierr.ProvenanceExecLogin && s.TokenAuthConfig == "corp-ldap" {
			s.TokenAuthConfig = ""
			cfg.Servers[name] = s
		}
	}
	assert.Equal(t, []string{"exec-login-r.ex-a", "exec-login-r.ex-b", "exec-login-r.ex-d", "exec-login-r.ex-e"}, cfg.AnsweringPluginEntries(srv, "u-alice", "corp-ldap"),
		"with none of the auth config, the unrecorded ones answer")
	assert.Equal(t, []string{"exec-login-r.ex-a", "exec-login-r.ex-b", "exec-login-r.ex-c", "exec-login-r.ex-d", "exec-login-r.ex-e"}, cfg.AnsweringPluginEntries(srv, "u-alice", ""),
		"without an auth config every plugin login entry of the user answers")

	one := authConfigDirectory()
	for _, name := range []string{"exec-login-r.ex-d", "exec-login-r.ex-b", "exec-login-r.ex-e"} {
		delete(one.Servers, name)
	}
	assert.False(t, CacheBindingForIdentified("/d", one, srv, "prod", "u-alice", "corp-ldap").Refused(), "one entry of the auth config answers, beside an unrecorded one that is gone and one of another")
	two := authConfigDirectory()
	assert.True(t, CacheBindingForIdentified("/d", two, srv, "staging", "u-alice", "corp-ldap").Refused(), "two entries of the auth config refuse the fallback")
	for _, name := range []string{"exec-login-r.ex-d"} {
		delete(two.Servers, name)
	}
	assert.False(t, CacheBindingForIdentified("/d", two, srv, "staging", "u-alice", "corp-ldap").Refused(),
		"the unrecorded entries beside one that recorded the auth config are not counted")
	unrecorded := authConfigDirectory()
	for _, name := range []string{"exec-login-r.ex-a", "exec-login-r.ex-d"} {
		delete(unrecorded.Servers, name)
	}
	assert.True(t, CacheBindingForIdentified("/d", unrecorded, srv, "staging", "u-alice", "corp-ldap").Refused(),
		"with none of the auth config, two unrecorded entries refuse")
}

// TestOwnedGroupOrderPutsTheRecordedAuthConfigFirst asserts the choice order
// between the entries a call with an auth config lists: the ones that
// recorded it, then the ones that recorded none.
func TestOwnedGroupOrderPutsTheRecordedAuthConfigFirst(t *testing.T) {
	t.Parallel()
	cfg := authConfigDirectory()
	assert.Zero(t, cfg.OwnedGroupOrder("prod", "corp-ldap"))
	assert.Equal(t, 1, cfg.OwnedGroupOrder("staging", "corp-ldap"))
	assert.Equal(t, 1, cfg.OwnedGroupOrder("prod", ""), "without an auth config there is one group")
}

// TestARecordWithAnAuthConfigListsEachEntrysAuthConfig asserts the owner and
// identified records of a call with an auth config carry it, and list each
// entry with the auth config it recorded, the unrecorded included; a record
// of a call without one carries and lists none.
func TestARecordWithAnAuthConfigListsEachEntrysAuthConfig(t *testing.T) {
	t.Parallel()
	cfg := authConfigDirectory()
	const srv = "https://r.ex"
	owner := CacheBindingForOwner("/d", cfg, srv, "u-alice", "corp-ldap").record("tok", time.Time{}, "", srv, "c-1")
	assert.Equal(t, "corp-ldap", owner.AuthConfig)
	require.NotNil(t, owner.Entries)
	assert.Equal(t, []cachedEntry{
		{Name: "exec-login-r.ex-a", BindingID: "b-pa", AuthConfig: "corp-ldap"}, {Name: "exec-login-r.ex-b", BindingID: "b-pb"},
		{Name: "exec-login-r.ex-d", BindingID: "b-pd", AuthConfig: "corp-ldap"}, {Name: "exec-login-r.ex-e", BindingID: "b-pe"},
		{Name: "prod", BindingID: "b-prod", AuthConfig: "corp-ldap"}, {Name: "staging", BindingID: "b-staging"},
	}, *owner.Entries)

	identified := CacheBindingForIdentified("/d", authConfigDirectory(), srv, "prod", "u-alice", "openldap").record("tok", time.Time{}, "prod", srv, "c-1")
	assert.Equal(t, "openldap", identified.AuthConfig)
	require.NotNil(t, identified.Entries)
	assert.Equal(t, []cachedEntry{{Name: "exec-login-r.ex-b", BindingID: "b-pb"}, {Name: "exec-login-r.ex-c", BindingID: "b-pc", AuthConfig: "openldap"},
		{Name: "exec-login-r.ex-e", BindingID: "b-pe"}}, *identified.Entries)

	plain := CacheBindingForOwner("/d", cfg, srv, "u-alice", "").record("tok", time.Time{}, "", srv, "c-1")
	assert.Empty(t, plain.AuthConfig)
	for _, e := range *plain.Entries {
		assert.Empty(t, e.AuthConfig, "a record without an auth config lists none: %s", e.Name)
	}

	own := CacheBindingForLookup("/d", cfg, srv, "prod", "prod", "u-alice", "corp-ldap", false).record("tok", time.Time{}, "prod", srv, "c-1")
	assert.Equal(t, "corp-ldap", own.AuthConfig, "a record made from the identified entry's own credential carries it too")
}

// TestARecordWithAnAuthConfigIsServedOnlyToACallWithIt asserts a hit
// compares the auth config the key carries with the record's: a record made
// for one auth config never serves a call with another or none, and a
// record with none never serves a call with one.
func TestARecordWithAnAuthConfigIsServedOnlyToACallWithIt(t *testing.T) {
	t.Parallel()
	const srv = "https://r.ex"
	cfg := authConfigDirectory()
	anchor := &CacheAnchor{Dir: "/d", Stamp: "s", Cfg: stamped(cfg, "s")}
	withA := CacheBindingForOwner("/d", cfg, srv, "u-alice", "corp-ldap").record("tok", time.Time{}, "", srv, "c-1")
	without := CacheBindingForOwner("/d", cfg, srv, "u-alice", "").record("tok", time.Time{}, "", srv, "c-1")
	for _, tc := range []struct {
		name   string
		key    string
		rec    cachedCred
		served bool
	}{
		{"its own key", CacheKeyFor("", srv, "c-1", "u-alice", "corp-ldap"), withA, true},
		{"the key of another auth config", CacheKeyFor("", srv, "c-1", "u-alice", "openldap"), withA, false},
		{"the key of a call with no auth config", CacheKeyFor("", srv, "c-1", "u-alice", ""), withA, false},
		{"the key of a call with an auth config, the record carrying none", CacheKeyFor("", srv, "c-1", "u-alice", "corp-ldap"), without, false},
		{"the key of a call with no auth config, the record carrying none", CacheKeyFor("", srv, "c-1", "u-alice", ""), without, true},
	} {
		assert.Equal(t, tc.served, anchor.validFor(tc.key, tc.rec), tc.name)
	}
}

// TestAnEntryThatRecordsAnAuthConfigEndsTheRecordsThatListedItWithoutOne
// asserts a record with an auth config lists each entry with the auth config
// it recorded, and the hit compares that beside the name and the id: an
// entry that has recorded one since stops the record, though its id is
// what it was. A record without an auth config does not look.
func TestAnEntryThatRecordsAnAuthConfigEndsTheRecordsThatListedItWithoutOne(t *testing.T) {
	t.Parallel()
	const srv = "https://r.ex"
	anchorFor := func(c *Config) *CacheAnchor { return &CacheAnchor{Dir: "/d", Stamp: "s", Cfg: stamped(c, "s")} }
	// One entry of the auth config answers as the fallback.
	oneAnswers := func() *Config {
		c := authConfigDirectory()
		delete(c.Servers, "exec-login-r.ex-d")
		return c
	}
	for _, kind := range []struct {
		name      string
		directory func() *Config
		record    func(*Config) cachedCred
		change    func(*Config, string)
	}{
		{"the owner record", authConfigDirectory, func(c *Config) cachedCred {
			return CacheBindingForOwner("/d", c, srv, "u-alice", "corp-ldap").record("tok", time.Time{}, "", srv, "c-1")
		}, func(c *Config, v string) { s := c.Servers["staging"]; s.TokenAuthConfig = v; c.Servers["staging"] = s }},
		{"the identified record", oneAnswers, func(c *Config) cachedCred {
			return CacheBindingForIdentified("/d", c, srv, "prod", "u-alice", "corp-ldap").record("tok", time.Time{}, "prod", srv, "c-1")
		}, func(c *Config, v string) {
			s := c.Servers["exec-login-r.ex-b"]
			s.TokenAuthConfig = v
			c.Servers["exec-login-r.ex-b"] = s
		}},
	} {
		t.Run(kind.name, func(t *testing.T) {
			rec := kind.record(kind.directory())
			assert.True(t, anchorFor(kind.directory()).valid(rec), "nothing changed")
			for _, v := range []string{"corp-ldap", "openldap"} {
				c := kind.directory()
				kind.change(c, v)
				assert.False(t, anchorFor(c).valid(rec), "an entry that listed none recorded %s", v)
			}
		})
	}
	// A record of a call without an auth config lists no auth configs, so it
	// ignores them.
	plain := CacheBindingForOwner("/d", authConfigDirectory(), srv, "u-alice", "").record("tok", time.Time{}, "", srv, "c-1")
	changed := authConfigDirectory()
	s := changed.Servers["staging"]
	s.TokenAuthConfig = "corp-ldap"
	changed.Servers["staging"] = s
	assert.True(t, anchorFor(changed).valid(plain))
}

// TestAnEntrysRecordedAuthConfigMovesTheBindingStamp asserts a config write
// that changes the auth config an entry records advances the binding stamp,
// as a change of its owner does, so a hit whose load came before it is a
// miss; a write of the same value does not.
func TestAnEntrysRecordedAuthConfigMovesTheBindingStamp(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		stamped bool
	}{
		{"an auth config is recorded", "corp-ldap", true},
		{"the same value is written", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, cfg, before := seedStampedConfig(t)
			e := cfg.Servers["prod"]
			e.TokenAuthConfig = tc.value
			cfg.Servers["prod"] = e
			require.NoError(t, cfg.Write())
			after := stampOnDisk(t, dir)
			if tc.stamped {
				assert.NotEqual(t, before, after)
				return
			}
			assert.Equal(t, before, after)
		})
	}
}

// recordFixture is a directory with prod, alice's entry with a cache binding
// id and no recorded auth config, and the binding of a call with --user-id
// u-alice and the auth config corp-ldap loaded from it.
func recordFixture(t *testing.T, mutate func(*Config)) (dir string, binding CacheBinding) {
	t.Helper()
	keyring.MockInit()
	t.Setenv(EnvCredentialStore, "file")
	dir = t.TempDir()
	cfg := &Config{CurrentServer: "prod", TokenReuse: true, CredentialStore: "file", Servers: map[string]Server{
		"prod": {URL: "https://r.ex", CacheBindingID: "b-prod", TokenUserID: "u-alice"},
	}}
	if mutate != nil {
		mutate(cfg)
	}
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	loaded, err := Load(FilePath(dir))
	require.NoError(t, err)
	return dir, CacheBindingForOwner(CanonicalDir(dir), loaded, "https://r.ex", "u-alice", "corp-ldap").ReadFrom(NewFileStore(dir))
}

func recordedAuthConfig(t *testing.T, dir string) string {
	t.Helper()
	loaded, err := Load(FilePath(dir))
	require.NoError(t, err)
	return loaded.Servers["prod"].TokenAuthConfig
}

// TestRecordAuthConfigWritesOnlyThatFieldOfThatEntry asserts the write
// that records an auth config on an entry that recorded none: it sets that
// one field, advances the stamp, leaves the cache binding id alone, and
// gives back a binding whose list carries the recorded value, so the record
// the call writes next lists it as it now stands.
func TestRecordAuthConfigWritesOnlyThatFieldOfThatEntry(t *testing.T) {
	dir, b := recordFixture(t, nil)
	before := stampOnDisk(t, dir)
	got, wrote := b.RecordAuthConfig(dir, "prod", "corp-ldap")
	require.True(t, wrote)
	assert.Equal(t, "corp-ldap", recordedAuthConfig(t, dir))
	loaded, err := Load(FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "b-prod", loaded.Servers["prod"].CacheBindingID, "the credential did not change, so neither does the id")
	assert.Equal(t, "u-alice", loaded.Servers["prod"].TokenUserID)
	assert.NotEqual(t, before, stampOnDisk(t, dir), "a change of what a record lists moves the stamp")
	rec := got.record("tok", time.Time{}, "", "https://r.ex", "c-1")
	require.NotNil(t, rec.Entries)
	assert.Equal(t, []cachedEntry{{Name: "prod", BindingID: "b-prod", AuthConfig: "corp-ldap"}}, *rec.Entries, "the next record lists the value just recorded")
	assert.Equal(t, "corp-ldap", rec.AuthConfig)
}

// TestRecordAuthConfigIsSkippedWhenAnyGuardFails asserts the write runs only
// when the entry's cache binding id in the config under the lock is the one
// the call's load showed, the load did not show its cache writes paused, the
// binding passes the store check, the entry still records no auth config and
// still records an owner, and the config lock is free to take at once: every
// other state writes nothing and says nothing.
func TestRecordAuthConfigIsSkippedWhenAnyGuardFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		// mutate changes the directory before the binding is loaded.
		mutate func(*Config)
		// after changes it between the load and the write.
		after func(t *testing.T, dir string)
		// cleanup runs after the write.
		hold func(t *testing.T, dir string) func()
		env  map[string]string
	}{
		{name: "the entry's cache binding id changed after the load", after: func(t *testing.T, dir string) {
			t.Helper()
			c, err := Load(FilePath(dir))
			require.NoError(t, err)
			s := c.Servers["prod"]
			s.CacheBindingID = "b-prod-2"
			c.Servers["prod"] = s
			require.NoError(t, c.Write())
		}},
		{name: "the load showed the entry's cache writes paused", mutate: func(c *Config) {
			s := c.Servers["prod"]
			s.CacheWritesPaused = PauseLocked
			c.Servers["prod"] = s
		}},
		{name: "the entry recorded an auth config after the load", after: func(t *testing.T, dir string) {
			t.Helper()
			c, err := Load(FilePath(dir))
			require.NoError(t, err)
			s := c.Servers["prod"]
			s.TokenAuthConfig = "openldap"
			c.Servers["prod"] = s
			require.NoError(t, c.Write())
		}},
		{name: "the entry recorded an auth config in the load", mutate: func(c *Config) {
			s := c.Servers["prod"]
			s.TokenAuthConfig = "openldap"
			c.Servers["prod"] = s
		}},
		{name: "the entry's recorded owner was cleared after the load", after: func(t *testing.T, dir string) {
			t.Helper()
			c, err := Load(FilePath(dir))
			require.NoError(t, err)
			s := c.Servers["prod"]
			s.TokenUserID = ""
			c.Servers["prod"] = s
			require.NoError(t, c.Write())
		}},
		{name: "the entry is gone", after: func(t *testing.T, dir string) {
			t.Helper()
			c, err := Load(FilePath(dir))
			require.NoError(t, err)
			delete(c.Servers, "prod")
			require.NoError(t, c.Write())
		}},
		{name: "the config lock is held", hold: func(t *testing.T, dir string) func() {
			t.Helper()
			held, release := make(chan struct{}), make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = WithLock(t.Context(), dir, func() error {
					close(held)
					<-release
					return nil
				})
			}()
			<-held
			return func() { close(release); <-done }
		}},
		{name: "the config lock is skipped by the environment", env: map[string]string{EnvConfigNoLock: "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, b := recordFixture(t, tc.mutate)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if tc.after != nil {
				tc.after(t, dir)
			}
			if tc.hold != nil {
				defer tc.hold(t, dir)()
			}
			was := ""
			if loaded, err := Load(FilePath(dir)); err == nil {
				was = loaded.Servers["prod"].TokenAuthConfig
			}
			got, wrote := b.RecordAuthConfig(dir, "prod", "corp-ldap")
			assert.False(t, wrote)
			assert.Equal(t, b, got, "the binding is as it was")
			if loaded, err := Load(FilePath(dir)); err == nil {
				assert.Equal(t, was, loaded.Servers["prod"].TokenAuthConfig, "the entry is as it was")
			}
		})
	}
}

// TestRecordAuthConfigTriesTheLockOnceAndNeverWaits asserts a busy config
// lock is not waited for: the call returns with nothing written while
// another holder keeps the lock, and it does not wait for the holder to let
// go.
func TestRecordAuthConfigTriesTheLockOnceAndNeverWaits(t *testing.T) {
	dir, b := recordFixture(t, nil)
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = WithLock(t.Context(), dir, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	returned := make(chan bool, 1)
	go func() {
		_, wrote := b.RecordAuthConfig(dir, "prod", "corp-ldap")
		returned <- wrote
	}()
	select {
	case wrote := <-returned:
		assert.False(t, wrote)
	case <-time.After(10 * time.Second):
		t.Error("the write waited for the config lock")
	}
	close(release)
	<-done
	assert.Empty(t, recordedAuthConfig(t, dir))
}

// TestRecordAuthConfigRefusesABindingThatCannotBeChecked asserts a binding no
// constructor built, one the lookup refuses, and one whose store check fails
// write nothing.
func TestRecordAuthConfigRefusesABindingThatCannotBeChecked(t *testing.T) {
	dir, b := recordFixture(t, nil)
	_, wrote := CacheBinding{}.RecordAuthConfig(dir, "prod", "corp-ldap")
	assert.False(t, wrote, "a binding no constructor built")
	_, wrote = CacheBinding{refused: true}.RecordAuthConfig(dir, "prod", "corp-ldap")
	assert.False(t, wrote, "a binding the lookup refuses")
	_, wrote = b.RecordAuthConfig(dir, "prod", "")
	assert.False(t, wrote, "nothing to record")
	_, wrote = b.RecordAuthConfig(dir, "staging", "corp-ldap")
	assert.False(t, wrote, "an entry the binding is not bound to")
	staleStore := b
	staleStore.from, staleStore.source = nil, "keychain"
	_, wrote = staleStore.RecordAuthConfig(dir, "prod", "corp-ldap")
	assert.False(t, wrote, "the store the credential was read from is not the one readers resolve now")
	assert.Empty(t, recordedAuthConfig(t, dir))
}

// TestAFailedRecordAuthConfigWriteIsNoWrite asserts a write that fails after
// the checks passed leaves the entry as it was and reports no write, so the
// call goes on and a later miss tries again.
func TestAFailedRecordAuthConfigWriteIsNoWrite(t *testing.T) {
	dir, b := recordFixture(t, nil)
	var steps []string
	restore := SetPersistWriteHookForTest(func(step string) error {
		steps = append(steps, step)
		return os.ErrPermission
	})
	got, wrote := b.RecordAuthConfig(dir, "prod", "corp-ldap")
	restore()
	assert.Equal(t, []string{"recordAuthConfig"}, steps)
	assert.False(t, wrote)
	assert.Equal(t, b, got)
	assert.Empty(t, recordedAuthConfig(t, dir))
	_, wrote = b.RecordAuthConfig(dir, "prod", "corp-ldap")
	assert.True(t, wrote, "a later miss tries again")
}

// TestRecordAuthConfigNeedsTheEntryInTheBindingsListOrIdentity asserts the
// binding finds the entry it is bound to in either place: the one it is
// bound to by id (a record made from the identified entry's own credential)
// and a list entry (the others).
func TestRecordAuthConfigNeedsTheEntryInTheBindingsListOrIdentity(t *testing.T) {
	dir, _ := recordFixture(t, nil)
	loaded, err := Load(FilePath(dir))
	require.NoError(t, err)
	own := CacheBindingFor(CanonicalDir(dir), loaded, "prod").ReadFrom(NewFileStore(dir))
	id, ok := own.BindingIDOf("prod")
	assert.True(t, ok)
	assert.Equal(t, "b-prod", id)
	got, wrote := own.RecordAuthConfig(dir, "prod", "corp-ldap")
	assert.True(t, wrote)
	assert.Equal(t, "corp-ldap", got.entry.authConfig)
	_, ok = own.BindingIDOf("staging")
	assert.False(t, ok)
}

// TestAPersistedLoginRecordsTheAuthConfigOfItsCreate asserts a login records
// the auth config its token create named on the entry, replaces the one the
// entry recorded before - a pasted token's login, which names none, clears
// it - and tells the caller the one it replaced.
func TestAPersistedLoginRecordsTheAuthConfigOfItsCreate(t *testing.T) {
	prior := derivedEntry()
	prior.TokenAuthConfig = "openldap"
	dir, _ := persistFixture(t, prior)
	store := NewFileStore(dir)
	in := loginInput(dir, store)
	in.TokenAuthConfig = "corp-ldap"
	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.NoError(t, err)
	assert.Equal(t, "openldap", out.ReplacedAuthConfig)
	loaded, err := Load(FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "corp-ldap", loaded.Servers["prod"].TokenAuthConfig)

	pasted := loginInput(dir, store)
	pasted.Provenance, pasted.TokenRef, pasted.TokenAuthConfig = clierr.ProvenanceUserSupplied, nil, ""
	out, err = PersistLoginCredential(t.Context(), dir, pasted)
	require.NoError(t, err)
	assert.Equal(t, "corp-ldap", out.ReplacedAuthConfig)
	loaded, err = Load(FilePath(dir))
	require.NoError(t, err)
	assert.Empty(t, loaded.Servers["prod"].TokenAuthConfig, "a pasted token records none and clears the previous one")

	dir2, _ := persistFixture(t, nil)
	in = pluginInput(dir2, "exec-login-r.ex-u-corp-ldap", NewFileStore(dir2))
	in.TokenAuthConfig = "corp-ldap"
	out, err = PersistLoginCredential(t.Context(), dir2, in)
	require.NoError(t, err)
	assert.Empty(t, out.ReplacedAuthConfig, "an entry the login created replaced nothing")
	created, err := Load(FilePath(dir2))
	require.NoError(t, err)
	assert.Equal(t, "corp-ldap", created.Servers[out.Name].TokenAuthConfig)
}

// TestAFailedLoginWritesTheRecordedAuthConfigBackWithTheOwner asserts the
// entry a failed login restores holds the auth config it recorded before,
// and one the login never had a credential for loses it with the owner
// when the old credential is dropped.
func TestAFailedLoginWritesTheRecordedAuthConfigBackWithTheOwner(t *testing.T) {
	t.Parallel()
	snap := derivedEntry()
	snap.TokenAuthConfig = "corp-ldap"
	kept := restoredEntry(snap, nil, false)
	assert.Equal(t, "corp-ldap", kept.TokenAuthConfig)
	assert.Equal(t, "user-a", kept.TokenUserID)
	dropped := restoredEntry(snap, nil, true)
	assert.Empty(t, dropped.TokenAuthConfig, "the auth config goes with the owner")
	assert.Empty(t, dropped.TokenUserID)
	assert.Nil(t, dropped.TokenRef)
}

// TestTheBindingOverAPluginLoginCarriesTheAuthConfigAndTheWrittenEntrysOne
// asserts the binding of the record the plugin-created login writes carries
// the call's auth config and lists the entry the login wrote with the auth
// config that write recorded, not the one the lookup's load showed.
func TestTheBindingOverAPluginLoginCarriesTheAuthConfigAndTheWrittenEntrysOne(t *testing.T) {
	dir, _ := persistFixture(t, nil)
	const srv = "https://r.ex"
	loaded, err := Load(FilePath(dir))
	require.NoError(t, err)
	lookup := CacheBindingForLookup(CanonicalDir(dir), loaded, srv, "", "", "user-a", "corp-ldap", false)
	in := pluginInput(dir, "exec-login-r.ex-user-a-corp-ldap", NewFileStore(dir))
	in.TokenAuthConfig = "corp-ldap"
	out, err := PersistLoginCredential(t.Context(), dir, in)
	require.NoError(t, err)
	over := out.CacheBindingOver(CanonicalDir(dir), lookup)
	assert.Equal(t, "corp-ldap", over.authConfig)
	var listed *boundEntry
	for i := range over.list {
		if over.list[i].name == out.Name {
			listed = &over.list[i]
		}
	}
	require.NotNil(t, listed, "the entry the login wrote is on the list")
	assert.Equal(t, "corp-ldap", listed.authConfig)
	rec := over.record("tok", time.Time{}, "", srv, "c-1")
	assert.Equal(t, "corp-ldap", rec.AuthConfig)

	// A call whose --name identified the entry the login wrote gets the record
	// of the entry's own credential, which carries the auth config too.
	dir2, _ := persistFixture(t, nil)
	const name = "exec-login-r.ex-user-a-corp-ldap"
	seeded, err := Load(FilePath(dir2))
	require.NoError(t, err)
	seeded.Servers[name] = Server{URL: srv, Provenance: clierr.ProvenanceExecLogin, TokenUserID: "user-a", CacheBindingID: "b-own"}
	require.NoError(t, seeded.Write())
	loaded2, err := Load(FilePath(dir2))
	require.NoError(t, err)
	identified := CacheBindingForLookup(CanonicalDir(dir2), loaded2, srv, name, "", "user-a", "corp-ldap", false)
	in2 := pluginInput(dir2, name, NewFileStore(dir2))
	in2.TokenAuthConfig = "corp-ldap"
	out2, err := PersistLoginCredential(t.Context(), dir2, in2)
	require.NoError(t, err)
	own := out2.CacheBindingOver(CanonicalDir(dir2), identified)
	assert.Equal(t, "corp-ldap", own.authConfig)
	assert.Equal(t, "corp-ldap", own.record("tok", time.Time{}, name, srv, "c-1").AuthConfig)
}

// TestACachedCredentialListingShowsTheRecordsAuthConfig asserts the listing
// of cached credentials shows the auth config the record serves, and none
// for a record of a call without one.
func TestACachedCredentialListingShowsTheRecordsAuthConfig(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir)
	cfg := authConfigDirectory()
	for name, a := range map[string]string{"with": "corp-ldap", "without": ""} {
		key := CacheKeyFor(name, "https://r.ex", "c-1", "u-alice", a)
		require.NoError(t, store.SetCachedCred(key, "tok-"+name, time.Now().Add(time.Hour), name, "https://r.ex", "c-1",
			CacheBindingForOwner(CanonicalDir(dir), cfg, "https://r.ex", "u-alice", a)))
	}
	infos, err := store.ListCachedCreds()
	require.NoError(t, err)
	got := map[string]string{}
	for _, i := range infos {
		got[i.Name] = i.TokenAuthConfig
	}
	assert.Equal(t, map[string]string{"with": "corp-ldap", "without": ""}, got)
}

// TestExecLoginEntryNameForNamesTheAuthConfigToo asserts a plugin login
// entry's name carries the auth config after the user id, and the name of a
// call with a user and no auth config, and of one with no user, is what it was.
func TestExecLoginEntryNameForNamesTheAuthConfigToo(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "exec-login-r.example.com-u-abc12-corp-ldap", ExecLoginEntryNameFor("https://r.example.com", "u-abc12", "corp-ldap"))
	assert.Equal(t, "exec-login-r.example.com-8443-u-abc12-corp-ldap", ExecLoginEntryNameFor("https://r.example.com:8443", "u-abc12", "corp-ldap"))
	assert.Equal(t, "exec-login-r.example.com-u-abc12", ExecLoginEntryNameFor("https://r.example.com", "u-abc12", ""))
	assert.Equal(t, "exec-login-r.example.com", ExecLoginEntryNameFor("https://r.example.com", "", "corp-ldap"), "an auth config applies with a user id only")
	assert.NotNil(t, ValidateEntryName(ExecLoginEntryNameFor("https://r.example.com", "u-abc12", "corp-ldap")), "the reserved prefix keeps it out of auth login")
	assert.Equal(t, ExecLoginEntryNameFor("https://r.example.com", "u-a", "b"), ExecLoginEntryNameFor("https://r.example.com", "u", "a-b"),
		"two pairs can give one name: the lookups choose by what the entry records, never by its name")
}
