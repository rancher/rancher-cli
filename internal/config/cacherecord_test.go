package config

import (
	"errors"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordConfig is a directory's config with two user entries and the
// plugin's own login entry at one URL, and an entry at another.
func recordConfig() *Config {
	return &Config{CurrentServer: "prod", TokenReuse: true, Servers: map[string]Server{
		"prod":              {URL: "https://r.ex", CacheBindingID: "b-prod"},
		"staging":           {URL: "https://r.ex", CacheBindingID: "b-staging"},
		"exec-login-r.ex":   {URL: "https://r.ex", CacheBindingID: "b-plugin", Provenance: clierr.ProvenanceExecLogin},
		"elsewhere":         {URL: "https://other.ex", CacheBindingID: "b-else"},
		"exec-login-other1": {URL: "https://other.ex", CacheBindingID: "b-plugin-else", Provenance: clierr.ProvenanceExecLogin},
	}}
}

// TestEachBindingIsBoundToWhatItsLookupReads asserts the record each
// constructor writes: the identified entry's own credential binds that
// entry alone, with the setting for a plugin login entry; any other record
// of an identified call binds that entry, the setting and, with it on, the
// URL's plugin login entries exactly; a call that identifies none binds
// every entry for the URL exactly, the current server among them and the
// setting, and the legacy shim's leaves the current server out.
func TestEachBindingIsBoundToWhatItsLookupReads(t *testing.T) {
	t.Parallel()
	on, off := true, false
	cases := []struct {
		name  string
		build func(cfg *Config) CacheBinding
		want  cachedCred
	}{
		{
			name:  "the identified entry's own credential",
			build: func(cfg *Config) CacheBinding { return CacheBindingFor("/d", cfg, "prod") },
			want:  cachedCred{Kind: recordOwnCredential, Identified: "prod", Entry: "prod", BindingID: "b-prod"},
		},
		{
			name:  "a plugin login entry's own credential",
			build: func(cfg *Config) CacheBinding { return CacheBindingFor("/d", cfg, "exec-login-r.ex") },
			want:  cachedCred{Kind: recordOwnCredential, Identified: "exec-login-r.ex", Entry: "exec-login-r.ex", BindingID: "b-plugin", TokenReuse: &on},
		},
		{
			name: "another record of an identified call, the setting on",
			build: func(cfg *Config) CacheBinding {
				return CacheBindingForIdentified("/d", cfg, "https://r.ex", "prod", "")
			},
			want: cachedCred{Kind: recordIdentified, Identified: "prod", Entry: "prod", BindingID: "b-prod", TokenReuse: &on,
				Entries: &[]cachedEntry{{Name: "exec-login-r.ex", BindingID: "b-plugin"}}},
		},
		{
			name: "another record of an identified call, the setting off",
			build: func(cfg *Config) CacheBinding {
				cfg.TokenReuse = false
				return CacheBindingForIdentified("/d", cfg, "https://r.ex", "prod", "")
			},
			want: cachedCred{Kind: recordIdentified, Identified: "prod", Entry: "prod", BindingID: "b-prod", TokenReuse: &off},
		},
		{
			name:  "a call that identifies no entry",
			build: func(cfg *Config) CacheBinding { return CacheBindingForServer("/d", cfg, "https://r.ex", false) },
			want: cachedCred{Kind: recordServer, CurrentServer: new("prod"), TokenReuse: &on, Entries: &[]cachedEntry{
				{Name: "exec-login-r.ex", BindingID: "b-plugin"}, {Name: "prod", BindingID: "b-prod"}, {Name: "staging", BindingID: "b-staging"},
			}},
		},
		{
			name:  "a call that identifies no entry, the current server at another URL",
			build: func(cfg *Config) CacheBinding { return CacheBindingForServer("/d", cfg, "https://other.ex", false) },
			want: cachedCred{Kind: recordServer, CurrentServer: new(""), TokenReuse: &on, Entries: &[]cachedEntry{
				{Name: "elsewhere", BindingID: "b-else"}, {Name: "exec-login-other1", BindingID: "b-plugin-else"},
			}},
		},
		{
			name:  "the legacy shim",
			build: func(cfg *Config) CacheBinding { return CacheBindingForServer("/d", cfg, "https://r.ex", true) },
			want: cachedCred{Kind: recordServer, TokenReuse: &on, Entries: &[]cachedEntry{
				{Name: "exec-login-r.ex", BindingID: "b-plugin"}, {Name: "prod", BindingID: "b-prod"}, {Name: "staging", BindingID: "b-staging"},
			}},
		},
		{
			name:  "a server the directory has no entry for",
			build: func(cfg *Config) CacheBinding { return CacheBindingForServer("/d", cfg, "https://none.ex", false) },
			want:  cachedCred{Kind: recordServer, CurrentServer: new(""), TokenReuse: &on, Entries: &[]cachedEntry{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := tc.build(recordConfig())
			require.NoError(t, b.writable())
			got := b.record("tok", time.Time{}, "n", "https://r.ex", "c")
			tc.want.Token, tc.want.Name, tc.want.Server, tc.want.Cluster = "tok", "n", "https://r.ex", "c"
			tc.want.Version, tc.want.Dir = cacheRecordVersion, "/d"
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("an entry the load does not have", func(t *testing.T) {
		t.Parallel()
		require.ErrorIs(t, CacheBindingFor("/d", recordConfig(), "gone").writable(), errCacheBindingUnset)
		require.ErrorIs(t, CacheBindingForIdentified("/d", recordConfig(), "https://r.ex", "gone", "").writable(), errCacheBindingUnset)
		require.ErrorIs(t, CacheBindingForServer("/d", nil, "https://r.ex", false).writable(), errCacheBindingUnset)
	})
}

// TestAWriteIsRefusedWhileAnyBoundEntryIsPaused asserts that a record is
// written only when none of the entries it is bound to was paused in the load,
// and that the refusal names the paused entry, for the entry bound by its
// id and for one in the list.
func TestAWriteIsRefusedWhileAnyBoundEntryIsPaused(t *testing.T) {
	t.Parallel()
	pause := func(name string) *Config {
		cfg := recordConfig()
		s := cfg.Servers[name]
		s.CacheWritesPaused = PauseLocked
		cfg.Servers[name] = s
		return cfg
	}
	cases := []struct {
		name   string
		b      CacheBinding
		paused string
	}{
		{"the identified entry", CacheBindingForIdentified("/d", pause("prod"), "https://r.ex", "prod", ""), "prod"},
		{"the plugin's login entry an identified call is bound to", CacheBindingForIdentified("/d", pause("exec-login-r.ex"), "https://r.ex", "prod", ""), "exec-login-r.ex"},
		{"another entry for the URL of a call that identifies none", CacheBindingForServer("/d", pause("staging"), "https://r.ex", false), "staging"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.b.writable()
			require.ErrorIs(t, err, ErrCacheWritesPaused)
			pe, ok := errors.AsType[*CacheWritesPausedError](err)
			require.True(t, ok, "%T", err)
			assert.Equal(t, tc.paused, pe.Entry)
			require.ErrorContains(t, err, tc.paused)
		})
	}

	t.Run("an entry the record is not bound to", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, CacheBindingForIdentified("/d", pause("staging"), "https://r.ex", "prod", "").writable(),
			"an identified call's record is not bound to the other user entries for the URL")
		assert.NoError(t, CacheBindingFor("/d", pause("exec-login-r.ex"), "prod").writable(),
			"nor is a record of the identified entry's own credential bound to the plugin's login entry")
	})
}

// TestAPluginCreatedLoginsBindingAddsTheEntryItWrote asserts how the
// plugin-created login's record is built from its lookup's binding: the
// entry it wrote is bound with the id its last write wrote, replacing what
// the lookup's load showed for it, or added to the list when the login
// created it; every other entry keeps the id the lookup's load showed; a
// call that identified the entry the login wrote gets a record bound to
// that entry alone; and the store checked is the one the commit reached.
func TestAPluginCreatedLoginsBindingAddsTheEntryItWrote(t *testing.T) {
	t.Parallel()
	written := func(change func(cfg *Config)) *Config {
		cfg := recordConfig()
		change(cfg)
		return cfg
	}
	loginWrote := func(cfg *Config, name, id string) {
		s := cfg.Servers[name]
		s.URL, s.CacheBindingID, s.Provenance = "https://r.ex", id, clierr.ProvenanceExecLogin
		cfg.Servers[name] = s
	}

	t.Run("an existing entry, for an identified call", func(t *testing.T) {
		t.Parallel()
		lookup := CacheBindingForIdentified("/d", recordConfig(), "https://r.ex", "prod", "")
		out := PersistLoginOutcome{Name: "exec-login-r.ex", committedTo: backendFile, written: written(func(cfg *Config) {
			loginWrote(cfg, "exec-login-r.ex", "b-plugin-2")
			// Another process's login of prod after the lookup read it.
			p := cfg.Servers["prod"]
			p.CacheBindingID = "b-prod-2"
			cfg.Servers["prod"] = p
		})}
		b := out.CacheBindingOver("/d", lookup)
		got := b.record("tok", time.Time{}, "n", "https://r.ex", "c")
		assert.Equal(t, "b-prod", got.BindingID, "the identified entry keeps the id the lookup read")
		assert.Equal(t, &[]cachedEntry{{Name: "exec-login-r.ex", BindingID: "b-plugin-2"}}, got.Entries)
		assert.Equal(t, backendFile, b.source)
		assert.Same(t, out.written, b.load, "the store check resolves from the config the login wrote")
	})

	t.Run("a created entry, for a call that identifies none", func(t *testing.T) {
		t.Parallel()
		cfg := recordConfig()
		delete(cfg.Servers, "exec-login-r.ex")
		lookup := CacheBindingForServer("/d", cfg, "https://r.ex", false)
		out := PersistLoginOutcome{Name: "exec-login-r.ex", committedTo: backendFile, written: written(func(cfg *Config) {
			loginWrote(cfg, "exec-login-r.ex", "b-new")
		})}
		got := out.CacheBindingOver("/d", lookup).record("tok", time.Time{}, "n", "https://r.ex", "c")
		assert.Equal(t, &[]cachedEntry{
			{Name: "exec-login-r.ex", BindingID: "b-new"}, {Name: "prod", BindingID: "b-prod"}, {Name: "staging", BindingID: "b-staging"},
		}, got.Entries, "added in order")
	})

	t.Run("the entry the call identified", func(t *testing.T) {
		t.Parallel()
		lookup := CacheBindingForIdentified("/d", recordConfig(), "https://r.ex", "exec-login-r.ex", "")
		out := PersistLoginOutcome{Name: "exec-login-r.ex", committedTo: backendFile, written: written(func(cfg *Config) {
			loginWrote(cfg, "exec-login-r.ex", "b-plugin-2")
		})}
		on := true
		got := out.CacheBindingOver("/d", lookup).record("tok", time.Time{}, "n", "https://r.ex", "c")
		assert.Equal(t, cachedCred{Token: "tok", Name: "n", Server: "https://r.ex", Cluster: "c", Version: cacheRecordVersion, Dir: "/d",
			Kind: recordOwnCredential, Identified: "exec-login-r.ex", Entry: "exec-login-r.ex", BindingID: "b-plugin-2", TokenReuse: &on}, got)
	})

	t.Run("a pause the login's outcome reports", func(t *testing.T) {
		t.Parallel()
		lookup := CacheBindingForIdentified("/d", recordConfig(), "https://r.ex", "prod", "")
		out := PersistLoginOutcome{Name: "exec-login-r.ex", CacheWritesPaused: true, committedTo: backendFile,
			written: written(func(cfg *Config) { loginWrote(cfg, "exec-login-r.ex", "b-plugin-2") })}
		err := out.CacheBindingOver("/d", lookup).writable()
		var pe *CacheWritesPausedError
		require.ErrorAs(t, err, &pe)
		assert.Equal(t, "exec-login-r.ex", pe.Entry)
	})

	t.Run("nothing to build from", func(t *testing.T) {
		t.Parallel()
		lookup := CacheBindingForIdentified("/d", recordConfig(), "https://r.ex", "prod", "")
		require.ErrorIs(t, PersistLoginOutcome{Name: "exec-login-r.ex"}.CacheBindingOver("/d", lookup).writable(), errCacheBindingUnset,
			"a login that returned before its last write")
		out := PersistLoginOutcome{Name: "exec-login-r.ex", written: recordConfig()}
		require.ErrorIs(t, out.CacheBindingOver("/d", CacheBinding{}).writable(), errCacheBindingUnset, "a lookup binding nobody built")
		off := recordConfig()
		off.TokenReuse = false
		require.ErrorIs(t, out.CacheBindingOver("/d", CacheBindingForIdentified("/d", off, "https://r.ex", "prod", "")).writable(), errCacheBindingUnset,
			"a lookup that reads no plugin login entry cannot hold the one the login wrote")
	})
}

// TestARecordMissingWhatItsKindIsBoundToIsAMiss asserts that the hit refuses a
// record that lacks a binding the rule for its kind has, rather than judging
// it by a weaker rule: an unnamed call's record without its list, the
// current server or the setting; an identified call's record whose entry
// is not the identified one; and a plugin login entry's own record without
// the setting.
func TestARecordMissingWhatItsKindIsBoundToIsAMiss(t *testing.T) {
	t.Parallel()
	cfg := recordConfig()
	a := &CacheAnchor{Dir: "/d", Cfg: cfg}
	full := func(b CacheBinding, name string) cachedCred {
		return b.record("tok", time.Time{}, name, "https://r.ex", "c")
	}
	unnamed := full(CacheBindingForServer("/d", cfg, "https://r.ex", false), "kube")
	require.True(t, a.valid(unnamed), "the control record is served")
	plugin := full(CacheBindingFor("/d", cfg, "exec-login-r.ex"), "exec-login-r.ex")
	require.True(t, a.valid(plugin), "the control record is served")
	identified := full(CacheBindingForIdentified("/d", cfg, "https://r.ex", "prod", ""), "prod")
	require.True(t, a.valid(identified), "the control record is served")
	offCfg := recordConfig()
	offCfg.TokenReuse = false
	offAnchor := &CacheAnchor{Dir: "/d", Cfg: offCfg}
	identifiedOff := CacheBindingForIdentified("/d", offCfg, "https://r.ex", "prod", "").record("tok", time.Time{}, "prod", "https://r.ex", "c")
	require.True(t, offAnchor.valid(identifiedOff), "the control record is served")
	own := full(CacheBindingFor("/d", cfg, "prod"), "prod")
	require.True(t, a.valid(own), "the control record is served")

	cases := []struct {
		name string
		rec  cachedCred
		drop func(r *cachedCred)
	}{
		{"an unnamed call's record without its list", unnamed, func(r *cachedCred) { r.Entries = nil }},
		{"an unnamed call's record without the current server", unnamed, func(r *cachedCred) { r.CurrentServer = nil }},
		{"an unnamed call's record without the setting", unnamed, func(r *cachedCred) { r.TokenReuse = nil }},
		{"an unnamed call's record naming an entry", unnamed, func(r *cachedCred) { r.Entry, r.BindingID = "prod", "b-prod" }},
		{"an identified call's record bound to another entry", identified, func(r *cachedCred) { r.Entry, r.BindingID = "staging", "b-staging" }},
		{"an identified call's list without the setting", identified, func(r *cachedCred) { r.TokenReuse = nil }},
		{"an identified call's record bound to the current server", identified, func(r *cachedCred) { s := "prod"; r.CurrentServer = &s }},
		{"a plugin login entry's own record without the setting", plugin, func(r *cachedCred) { r.TokenReuse = nil }},
		{"an identified call's record, the setting on, without its list", identified, func(r *cachedCred) { r.Entries = nil }},
		{"an own-credential record carrying a list", own, func(r *cachedCred) { r.Entries = &[]cachedEntry{} }},
		{"an own-credential record of a user entry bound to the setting", own, func(r *cachedCred) { on := true; r.TokenReuse = &on }},
		{"a record of no kind", own, func(r *cachedCred) { r.Kind = "" }},
		{"a record of a kind this binary does not know", unnamed, func(r *cachedCred) { r.Kind = "later" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := tc.rec
			if rec.Entries != nil {
				entries := *rec.Entries
				rec.Entries = &entries
			}
			tc.drop(&rec)
			assert.False(t, a.valid(rec))
		})
	}

	t.Run("an identified call's record, the setting off, without the setting", func(t *testing.T) {
		t.Parallel()
		rec := identifiedOff
		rec.TokenReuse = nil
		assert.False(t, offAnchor.valid(rec), "judged as its own kind, not as a record of the entry's own credential")
	})
}

// TestARecordFollowsTheLookupItWasWrittenFor asserts, in the package that
// judges records, the three comparisons a hit makes against the lookup of
// the record's call: the identification (a name that now identifies
// another entry, at the URL or at another, is a miss), every listed id (a
// listed entry whose id advanced is a miss), and the plugin login entries
// of an identified call (a second one appearing is a miss, and so is a
// record that lists two for a call identifying a user entry, which that
// lookup refuses; a call identifying a plugin login entry reads no other,
// so its record lists them and is served).
func TestARecordFollowsTheLookupItWasWrittenFor(t *testing.T) {
	t.Parallel()
	judge := func(cfg *Config, rec cachedCred) bool { return (&CacheAnchor{Dir: "/d", Cfg: cfg}).valid(rec) }
	rec := func(b CacheBinding, name string) cachedCred {
		require.True(t, b.built, "the control binding is built")
		return b.record("tok", time.Time{}, name, "https://r.ex", "c")
	}
	with := func(change func(cfg *Config)) *Config {
		cfg := recordConfig()
		change(cfg)
		return cfg
	}
	addEntry := func(name, url string, plugin bool) func(cfg *Config) {
		return func(cfg *Config) {
			s := Server{URL: url, CacheBindingID: "b-" + name}
			if plugin {
				s.Provenance = clierr.ProvenanceExecLogin
			}
			cfg.Servers[name] = s
		}
	}

	t.Run("the identification", func(t *testing.T) {
		t.Parallel()
		own := rec(CacheBindingFor("/d", recordConfig(), "prod"), "prod-c")
		require.True(t, judge(recordConfig(), own), "the control record is served")
		assert.False(t, judge(with(addEntry("prod-c", "https://r.ex", false)), own), "the name now identifies another entry at the URL")
		assert.False(t, judge(with(addEntry("prod-c", "https://other.ex", false)), own), "the name now identifies an entry at another URL")
		unnamed := rec(CacheBindingForServer("/d", recordConfig(), "https://r.ex", false), "kube-c")
		require.True(t, judge(recordConfig(), unnamed), "the control record is served")
		assert.False(t, judge(with(addEntry("kube-c", "https://other.ex", false)), unnamed), "a name that identified nothing now identifies an entry")
	})

	t.Run("an identified entry moved to another URL with its id", func(t *testing.T) {
		t.Parallel()
		own := rec(CacheBindingFor("/d", recordConfig(), "prod"), "prod-c")
		assert.False(t, judge(with(func(cfg *Config) {
			s := cfg.Servers["prod"]
			s.URL = "https://other.ex"
			cfg.Servers["prod"] = s
		}), own), "the lookup refuses a call whose entry is registered at another URL")
	})

	t.Run("the token-reuse setting", func(t *testing.T) {
		t.Parallel()
		off := with(func(cfg *Config) { cfg.TokenReuse = false })
		for name, r := range map[string]cachedCred{
			"a plugin login entry's own record": rec(CacheBindingFor("/d", recordConfig(), "exec-login-r.ex"), "exec-login-r.ex"),
			"an identified call's other record": rec(CacheBindingForIdentified("/d", recordConfig(), "https://r.ex", "prod", ""), "prod"),
			"an unnamed call's record":          rec(CacheBindingForServer("/d", recordConfig(), "https://r.ex", false), "kube-c"),
		} {
			require.True(t, judge(recordConfig(), r), "%s: the control record is served", name)
			assert.False(t, judge(off, r), "%s: the setting turned off", name)
		}
		own := rec(CacheBindingFor("/d", recordConfig(), "prod"), "prod")
		assert.True(t, judge(off, own), "a user entry's own record is not bound to the setting")
	})

	t.Run("the current server", func(t *testing.T) {
		t.Parallel()
		unnamed := rec(CacheBindingForServer("/d", recordConfig(), "https://r.ex", false), "kube-c")
		assert.False(t, judge(with(func(cfg *Config) { cfg.CurrentServer = "staging" }), unnamed),
			"the current server moved to another entry for the URL")
		away := with(func(cfg *Config) { cfg.CurrentServer = "elsewhere" })
		fromAway := rec(CacheBindingForServer("/d", away, "https://r.ex", false), "kube-c")
		assert.True(t, judge(with(func(cfg *Config) { cfg.CurrentServer = "exec-login-other1" }), fromAway),
			"a move between two entries at another URL changes nothing the lookup reads")
		shim := rec(CacheBindingForServer("/d", recordConfig(), "https://r.ex", true), LegacyCachePrefix+"alice")
		assert.True(t, judge(with(func(cfg *Config) { cfg.CurrentServer = "staging" }), shim),
			"the legacy shim ignores the current server")
	})

	t.Run("every listed id", func(t *testing.T) {
		t.Parallel()
		unnamed := rec(CacheBindingForServer("/d", recordConfig(), "https://r.ex", false), "kube-c")
		assert.False(t, judge(with(func(cfg *Config) {
			s := cfg.Servers["staging"]
			s.CacheBindingID = "b-staging-2"
			cfg.Servers["staging"] = s
		}), unnamed), "a listed entry's id advanced")
	})

	t.Run("an identified call's plugin login entries", func(t *testing.T) {
		t.Parallel()
		fallback := rec(CacheBindingForIdentified("/d", recordConfig(), "https://r.ex", "prod", ""), "prod")
		require.True(t, judge(recordConfig(), fallback), "the control record is served")
		two := with(addEntry("exec-login-second", "https://r.ex", true))
		assert.False(t, judge(two, fallback), "a second plugin login entry appeared")
		assert.False(t, judge(with(func(cfg *Config) {
			s := cfg.Servers["exec-login-r.ex"]
			s.CacheBindingID = "b-plugin-2"
			cfg.Servers["exec-login-r.ex"] = s
		}), fallback), "the plugin login entry's id advanced")
		assert.False(t, judge(with(func(cfg *Config) {
			delete(cfg.Servers, "exec-login-r.ex")
			addEntry("exec-login-second", "https://r.ex", true)(cfg)
		}), fallback), "another plugin login entry took its place")

		listed := fallback
		entries := []cachedEntry{{Name: "exec-login-r.ex", BindingID: "b-plugin"}, {Name: "exec-login-second", BindingID: "b-exec-login-second"}}
		listed.Entries = &entries
		assert.False(t, judge(two, listed), "a record listing both, for a call whose lookup refuses them")

		ofPlugin := rec(CacheBindingForIdentified("/d", two, "https://r.ex", "exec-login-r.ex", ""), "exec-login-r.ex")
		assert.True(t, judge(two, ofPlugin), "a call identifying a plugin login entry reads no other")
	})

	t.Run("the writers give no binding for a list the lookup refuses", func(t *testing.T) {
		t.Parallel()
		two := with(addEntry("exec-login-second", "https://r.ex", true))
		direct := CacheBindingForIdentified("/d", two, "https://r.ex", "prod", "")
		assert.True(t, direct.Refused())
		require.ErrorIs(t, direct.writable(), errCacheRecordRefused)

		lookup := CacheBindingForIdentified("/d", recordConfig(), "https://r.ex", "prod", "")
		out := PersistLoginOutcome{Name: "exec-login-second", committedTo: backendFile, written: two}
		b := out.CacheBindingOver("/d", lookup)
		assert.True(t, b.Refused(), "the login added the server's second plugin login entry")
		require.ErrorIs(t, b.writable(), errCacheRecordRefused)
		assert.False(t, CacheBinding{}.Refused(), "a binding nobody built is a failure, not a refusal")
	})
}
