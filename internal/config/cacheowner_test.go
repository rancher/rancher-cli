package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// stamped is c carrying the binding stamp s, as a load after the stamp was
// read does.
func stamped(c *Config, s string) *Config {
	c.BindingStamp = s
	return c
}

// ownerConfig is a directory with entries of two users for one URL: alice's
// prod and staging, with prod current, her plugin login entry, bob's
// entry and an entry that records no owner, and an entry at another URL.
func ownerConfig() *Config {
	return &Config{CurrentServer: "prod", TokenReuse: true, Servers: map[string]Server{
		"prod":              {URL: "https://r.ex", CacheBindingID: "b-prod", TokenUserID: "u-alice"},
		"staging":           {URL: "https://r.ex", CacheBindingID: "b-staging", TokenUserID: "u-alice"},
		"exec-login-r.ex":   {URL: "https://r.ex", CacheBindingID: "b-plugin", TokenUserID: "u-alice", Provenance: clierr.ProvenanceExecLogin},
		"bobs":              {URL: "https://r.ex", CacheBindingID: "b-bob", TokenUserID: "u-bob"},
		"unowned":           {URL: "https://r.ex", CacheBindingID: "b-unowned"},
		"elsewhere":         {URL: "https://other.ex", CacheBindingID: "b-else", TokenUserID: "u-alice"},
		"exec-login-r.ex-b": {URL: "https://r.ex", CacheBindingID: "b-plugin-bob", TokenUserID: "u-bob", Provenance: clierr.ProvenanceExecLogin},
	}}
}

// TestExecLoginEntryNameForNamesTheServerAndTheUser asserts a plugin login
// entry's name is the server's with the user id after it, the name of the
// server alone for a call without one, and that the longest name the
// credential store takes is the 500-character account less the 32-character
// id and the colon.
func TestExecLoginEntryNameForNamesTheServerAndTheUser(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "exec-login-r.example.com-u-abc12", ExecLoginEntryNameFor("https://r.example.com", "u-abc12", ""))
	assert.Equal(t, "exec-login-r.example.com-8443-u-abc12", ExecLoginEntryNameFor("https://r.example.com:8443", "u-abc12", ""))
	assert.Equal(t, ExecLoginEntryName("https://r.example.com"), ExecLoginEntryNameFor("https://r.example.com", "", ""))
	assert.Nil(t, ValidateEntryName("x"))
	assert.NotNil(t, ValidateEntryName(ExecLoginEntryNameFor("https://r.example.com", "u-abc12", "")), "the reserved prefix keeps it out of auth login")
	assert.Equal(t, 467, MaxEntryNameLength)
	assert.Equal(t, 500, len(keychainAccount(string(make([]byte, identityIDLen)), string(make([]byte, MaxEntryNameLength)))))
}

// TestTheOwnerRecordIsBoundToTheEntriesRecordingTheUser asserts the record
// of a call with --user-id and no --name: bound exactly to the entries for
// the URL that record the user, each with its id, to the current server
// among them and to the setting, and carrying the user id; an entry of
// another user or with no owner is not in it.
func TestTheOwnerRecordIsBoundToTheEntriesRecordingTheUser(t *testing.T) {
	t.Parallel()
	cfg := ownerConfig()
	on := true
	got := CacheBindingForOwner("/d", cfg, "https://r.ex", "u-alice", "").record("tok", time.Time{}, "", "https://r.ex", "c-1")
	current := "prod"
	assert.Equal(t, cachedCred{
		Token: "tok", Server: "https://r.ex", Cluster: "c-1", Version: cacheRecordVersion, Dir: "/d", Kind: recordOwner, UserID: "u-alice",
		Entries:       &[]cachedEntry{{Name: "exec-login-r.ex", BindingID: "b-plugin"}, {Name: "prod", BindingID: "b-prod"}, {Name: "staging", BindingID: "b-staging"}},
		CurrentServer: &current, TokenReuse: &on,
	}, got)

	noCurrent := ownerConfig()
	noCurrent.CurrentServer = "bobs"
	got = CacheBindingForOwner("/d", noCurrent, "https://r.ex", "u-alice", "").record("tok", time.Time{}, "", "https://r.ex", "c-1")
	require.NotNil(t, got.CurrentServer)
	assert.Empty(t, *got.CurrentServer, "the current server is bound only when it is one of the user's entries")

	assert.False(t, CacheBindingForOwner("/d", nil, "https://r.ex", "u-alice", "").built, "no config gives no binding")
	assert.False(t, CacheBindingForOwner("/d", cfg, "https://r.ex", "", "").built, "and no user")
	empty := CacheBindingForOwner("/d", cfg, "https://r.ex", "u-nobody", "").record("tok", time.Time{}, "", "https://r.ex", "c-1")
	require.NotNil(t, empty.Entries)
	assert.Empty(t, *empty.Entries, "a user with no entry has the empty list, which stays true until one appears")
}

// TestAnOwnerRecordIsServedOnlyWhileTheUsersEntriesAreAsTheyWere asserts
// each part of the owner record's binding is needed: the list of names,
// each entry's id, the exactness of the list (an entry that records the
// user appears, becomes the user's, or goes), the current server among the
// entries, the setting and the user id; and that a change of another
// user's entry leaves the record served.
func TestAnOwnerRecordIsServedOnlyWhileTheUsersEntriesAreAsTheyWere(t *testing.T) {
	t.Parallel()
	const server = "https://r.ex"
	dir := "/d"
	anchorFor := func(c *Config) *CacheAnchor { return &CacheAnchor{Dir: dir, Stamp: "s", Cfg: stamped(c, "s")} }
	record := func() cachedCred {
		return CacheBindingForOwner(dir, ownerConfig(), server, "u-alice", "").record("tok", time.Time{}, "", server, "c-1")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		served bool
	}{
		{"nothing changes", func(*Config) {}, true},
		{"another user's entry is added", func(c *Config) {
			c.Servers["more-bob"] = Server{URL: server, TokenUserID: "u-bob", CacheBindingID: "b-more"}
		}, true},
		{"another user's entry changes", func(c *Config) { s := c.Servers["bobs"]; s.CacheBindingID = "b-bob2"; c.Servers["bobs"] = s }, true},
		{"another user's entry goes", func(c *Config) { delete(c.Servers, "bobs") }, true},
		{"an entry at another URL changes", func(c *Config) { s := c.Servers["elsewhere"]; s.CacheBindingID = "x"; c.Servers["elsewhere"] = s }, true},
		{"an entry of the user's changes id", func(c *Config) { s := c.Servers["staging"]; s.CacheBindingID = "b-staging2"; c.Servers["staging"] = s }, false},
		{"the plugin login entry changes id", func(c *Config) {
			s := c.Servers["exec-login-r.ex"]
			s.CacheBindingID = "x"
			c.Servers["exec-login-r.ex"] = s
		}, false},
		{"an entry of the user's goes", func(c *Config) { delete(c.Servers, "staging") }, false},
		{"a new entry of the user's appears", func(c *Config) {
			c.Servers["more"] = Server{URL: server, TokenUserID: "u-alice", CacheBindingID: "b-more"}
		}, false},
		{"an entry with no owner becomes the user's", func(c *Config) { s := c.Servers["unowned"]; s.TokenUserID = "u-alice"; c.Servers["unowned"] = s }, false},
		{"another user's entry becomes the user's", func(c *Config) { s := c.Servers["bobs"]; s.TokenUserID = "u-alice"; c.Servers["bobs"] = s }, false},
		{"an entry of the user's becomes another user's", func(c *Config) { s := c.Servers["staging"]; s.TokenUserID = "u-bob"; c.Servers["staging"] = s }, false},
		{"the current server moves to another of the user's", func(c *Config) { c.CurrentServer = "staging" }, false},
		{"the current server moves off the user's entries", func(c *Config) { c.CurrentServer = "bobs" }, false},
		{"token reuse is turned off", func(c *Config) { c.TokenReuse = false }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ownerConfig()
			tc.mutate(cfg)
			assert.Equal(t, tc.served, anchorFor(cfg).valid(record()))
		})
	}
	t.Run("the record is judged by its own user id", func(t *testing.T) {
		rec := record()
		rec.UserID = "u-bob"
		assert.False(t, anchorFor(ownerConfig()).valid(rec), "bob's entries are not the list it carries")
		rec.UserID = ""
		assert.False(t, anchorFor(ownerConfig()).valid(rec), "an owner record with no user is never served")
	})
	t.Run("the shape of the kind", func(t *testing.T) {
		for name, mutate := range map[string]func(*cachedCred){
			"an entry bound by id":        func(r *cachedCred) { r.Entry, r.BindingID = "prod", "b-prod" },
			"an identified entry":         func(r *cachedCred) { r.Identified = "prod" },
			"no list":                     func(r *cachedCred) { r.Entries = nil },
			"no setting":                  func(r *cachedCred) { r.TokenReuse = nil },
			"no current server":           func(r *cachedCred) { r.CurrentServer = nil },
			"a name the lookup would use": func(r *cachedCred) { r.Name = "prod" },
		} {
			rec := record()
			mutate(&rec)
			assert.False(t, anchorFor(ownerConfig()).valid(rec), name)
		}
	})
}

// TestTheIdentifiedRecordListsOnlyThePluginLoginEntriesRecordingTheUser
// asserts the exact list of an identified call's record, for a call with
// --user-id, holds the plugin login entries that record that user, so
// another user's plugin login entry changing leaves it served and the
// user's own ends it; without --user-id it holds them all.
func TestTheIdentifiedRecordListsOnlyThePluginLoginEntriesRecordingTheUser(t *testing.T) {
	t.Parallel()
	const server = "https://r.ex"
	anchorFor := func(c *Config) *CacheAnchor { return &CacheAnchor{Dir: "/d", Stamp: "s", Cfg: stamped(c, "s")} }
	rec := func(user string) cachedCred {
		return CacheBindingForIdentified("/d", ownerConfig(), server, "prod", user, "").record("tok", time.Time{}, "prod", server, "c-1")
	}
	alice := rec("u-alice")
	assert.Equal(t, "u-alice", alice.UserID)
	require.NotNil(t, alice.Entries)
	assert.Equal(t, []cachedEntry{{Name: "exec-login-r.ex", BindingID: "b-plugin"}}, *alice.Entries)
	assert.True(t, CacheBindingForIdentified("/d", ownerConfig(), server, "prod", "", "").Refused(), "without --user-id two plugin login entries refuse the fallback")

	assert.True(t, anchorFor(ownerConfig()).valid(alice))
	other := ownerConfig()
	s := other.Servers["exec-login-r.ex-b"]
	s.CacheBindingID = "x"
	other.Servers["exec-login-r.ex-b"] = s
	assert.True(t, anchorFor(other).valid(alice), "another user's plugin login entry changing leaves the record served")
	own := ownerConfig()
	s = own.Servers["exec-login-r.ex"]
	s.CacheBindingID = "x"
	own.Servers["exec-login-r.ex"] = s
	assert.False(t, anchorFor(own).valid(alice))
	second := ownerConfig()
	second.Servers["exec-login-two"] = Server{URL: server, TokenUserID: "u-alice", CacheBindingID: "b2", Provenance: clierr.ProvenanceExecLogin}
	assert.False(t, anchorFor(second).valid(alice), "a second entry of the user's refuses the fallback")
}

// TestARecordIsServedOnlyUnderTheKeyOfItsUser asserts a record carries the
// user id its key carries: a record of a call with --user-id is not served
// under the key of a call without one, nor under another user's key, and a
// record with no user id is not served under a key that carries one.
func TestARecordIsServedOnlyUnderTheKeyOfItsUser(t *testing.T) {
	t.Parallel()
	cfg := ownerConfig()
	a := &CacheAnchor{Dir: "/d", Stamp: "s", Cfg: stamped(cfg, "s")}
	owned := CacheBindingForOwner("/d", cfg, "https://r.ex", "u-alice", "").record("tok", time.Time{}, "", "https://r.ex", "c-1")
	plain := CacheBindingForServer("/d", &Config{}, "https://none.ex", false).record("tok", time.Time{}, "kube", "https://none.ex", "c-1")
	for _, tc := range []struct {
		name   string
		key    string
		rec    cachedCred
		served bool
	}{
		{"its own key", CacheKeyFor("", "r.ex", "c-1", "u-alice", ""), owned, true},
		{"another user's key", CacheKeyFor("", "r.ex", "c-1", "u-bob", ""), owned, false},
		{"the key of a call without a user", CacheKeyFor("", "r.ex", "c-1", "", ""), owned, false},
		{"a user's key, the record carrying none", CacheKeyFor("kube", "none.ex", "c-1", "u-alice", ""), plain, false},
		{"a record with no user under its own key", CacheKeyFor("kube", "none.ex", "c-1", "", ""), plain, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.served {
				assert.True(t, userIDOfKey(tc.key) == tc.rec.UserID)
			}
			if tc.rec.Kind == recordOwner {
				assert.Equal(t, tc.served, a.validFor(tc.key, tc.rec))
				return
			}
			bare := &CacheAnchor{Dir: "/d", Stamp: "s", Cfg: stamped(&Config{}, "s")}
			assert.Equal(t, tc.served, bare.validFor(tc.key, tc.rec))
		})
	}
	assert.Equal(t, "u-abc12", userIDOfKey(CacheKeyFor("n", "r.ex", "c", "u-abc12", "")))
	assert.Empty(t, userIDOfKey(CacheKeyFor("n", "r.ex", "c", "", "")))
}

// TestAnOwnerRecordIsStoredAndReadBack asserts an owner record written
// through the store is read back for the same user and slot, and not for
// another.
func TestAnOwnerRecordIsStoredAndReadBack(t *testing.T) {
	dir := t.TempDir()
	cfg := ownerConfig()
	cfg.SetPath(FilePath(dir))
	require.NoError(t, cfg.Write())
	store := NewFileStore(dir)
	canon := CanonicalDir(dir)
	loaded, err := Load(FilePath(dir))
	require.NoError(t, err)
	key := CacheKeyFor("", "https://r.ex", "c-1", "u-alice", "")
	require.NoError(t, store.SetCachedCred(key, "tok", time.Now().Add(time.Hour), "", "https://r.ex", "c-1", CacheBindingForOwner(canon, loaded, "https://r.ex", "u-alice", "")))
	anchor := NewCacheAnchor(dir).WithConfig(loaded)
	got, _, err := store.GetCachedCred(key, anchor)
	require.NoError(t, err)
	assert.Equal(t, "tok", got)
	_, _, err = store.GetCachedCred(CacheKeyFor("", "https://r.ex", "c-1", "u-bob", ""), anchor)
	assert.ErrorIs(t, err, ErrNoToken)
}
