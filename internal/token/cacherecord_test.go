package token

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gofrs/flock"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
	"sigs.k8s.io/yaml"
)

// recordFixture is a directory and a fixture Rancher for the tests of
// which cache records a hit may serve: each call runs the exec plugin as
// kubectl would, and each change writes config.yaml and the store as the
// command that makes it does.
type recordFixture struct {
	t     *testing.T
	f     *testrancher.Facade
	srv   *httptest.Server
	dir   string
	store config.CredStore
}

func newRecordFixture(t *testing.T, reuse bool) *recordFixture {
	t.Helper()
	return newRecordFixtureWith(t, reuse, nil)
}

// newRecordFixtureWith is newRecordFixture with the fixture Rancher's
// handler wrapped by wrap, so a test can act between two requests of a
// call.
func newRecordFixtureWith(t *testing.T, reuse bool, wrap func(http.Handler) http.Handler) *recordFixture {
	t.Helper()
	keyring.MockInit()
	t.Setenv(execInfoEnv, "")
	t.Setenv(config.EnvCredentialStore, "")
	opts := testrancher.Options{Users: entryTableUsers()}
	f := newFacadeWithClusters(&opts)
	var h http.Handler = f
	if wrap != nil {
		h = wrap(f)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{}, TokenReuse: reuse, CredentialStore: "file"}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return &recordFixture{t: t, f: f, srv: srv, dir: dir, store: config.NewFileStore(dir)}
}

// edit loads config.yaml, applies change and writes it back.
func (r *recordFixture) edit(change func(cfg *config.Config)) {
	r.t.Helper()
	cfg, err := config.Load(config.FilePath(r.dir))
	require.NoError(r.t, err)
	change(cfg)
	require.NoError(r.t, cfg.Write())
}

// login gives entry a login of user at url, creating the entry when it
// does not exist: a fresh cache binding id, then the credential, as every
// login writes them. An empty url is the fixture's.
func (r *recordFixture) login(entry, url, user string, plugin bool) {
	r.t.Helper()
	if url == "" {
		url = r.srv.URL
	}
	r.edit(func(cfg *config.Config) {
		s := cfg.Servers[entry]
		s.URL = url
		if plugin {
			s.Provenance = clierr.ProvenanceExecLogin
		}
		require.NoError(r.t, config.AdvanceCacheBinding(&s))
		cfg.Servers[entry] = s
	})
	require.NoError(r.t, r.store.SetToken(config.ServerRef{Name: entry, URL: url}, acquireFor(r.t, r.srv, user)))
}

// call runs `auth get-token --name name --cluster c-test` against the
// fixture, with a terminal where carol signs in when asked, or with none,
// and returns the account the served token belongs to.
func (r *recordFixture) call(name string, terminal bool) (string, error) {
	r.t.Helper()
	return r.callTo(name, terminal, &bytes.Buffer{})
}

// callTo is call with the call's notices written to notices.
func (r *recordFixture) callTo(name string, terminal bool, notices *bytes.Buffer) (string, error) {
	r.t.Helper()
	p := ui.New(notices, strings.NewReader(""), false, false)
	if terminal {
		p = ui.New(notices, newLineReader("carol", "pw-carol"), false, true)
	}
	cmd := NewTokenCommand(r.dir, r.store, anchorFor(r.dir)).(runtime.ReadCommand)
	res, err := cmd.Execute(r.t.Context(), tin{s: map[string]string{
		"server": r.srv.URL, "cluster": "c-test", "name": name, "auth-provider": "localProvider",
	}}, p)
	if err != nil {
		return "", err
	}
	cred, ok := res.Data.(*execCredential)
	require.True(r.t, ok)
	owner, found := r.f.BearerOwner(cred.Status.Token)
	require.True(r.t, found, "the served token is one the server issued")
	return strings.TrimPrefix(owner, "user-"), nil
}

// hit reports whether the flag fast path serves a record for name.
func (r *recordFixture) hit(name string) bool {
	r.t.Helper()
	c := tokenCmd{dir: r.dir, store: r.store, anchor: anchorFor(r.dir)}
	_, _, _, hit := c.probeFlagCache(name, r.srv.URL, "c-test", ui.Discard())
	return hit
}

// TestCacheHitFollowsTheEntryTheNameIdentifies asserts that a cached
// credential is served only while the exec entry's --name identifies the
// entry the record was made for. An entry named exactly like the --name
// wins the identification once it exists: at the call's URL the call then
// uses that entry's login, and at another URL the call is refused, so the
// record of the earlier entry is never served again. A --name that
// identified no entry is refused the same way once an entry named like it
// exists at another URL.
func TestCacheHitFollowsTheEntryTheNameIdentifies(t *testing.T) {
	cases := []struct {
		name     string
		callName string
		url      string
		wantUser string
		wantCode string
	}{
		{name: "the new entry at the call's URL", callName: "prod-c-test", wantUser: "bob"},
		{name: "the new entry at another URL", callName: "prod-c-test", url: otherServerURL, wantCode: clierr.CodeConflict},
		{name: "an unidentified name, the new entry at another URL", callName: "kube-c-test", url: otherServerURL, wantCode: clierr.CodeConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRecordFixture(t, false)
			r.login("prod", "", "alice", false)
			r.edit(func(cfg *config.Config) { cfg.CurrentServer = "prod" })

			got, err := r.call(tc.callName, false)
			require.NoError(t, err)
			require.Equal(t, "alice", got)
			require.True(t, r.hit(tc.callName), "the call's record is served")

			r.login(tc.callName, tc.url, "bob", false)
			got, err = r.call(tc.callName, false)
			if tc.wantCode != "" {
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "want the typed refusal the uncached call gets, got user %q, error %v", got, err)
				assert.Equal(t, tc.wantCode, ce.Code)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantUser, got, "the call uses the entry its --name now identifies")
		})
	}
}

// loginAside is login for an existing entry at the fixture's URL, for a
// goroutine other than the test's: it returns what failed instead of
// failing the test.
func (r *recordFixture) loginAside(entry, user string) error {
	cred, err := AcquireLoginCredential(r.t.Context(), r.srv.Client(),
		LoginRequest{Server: r.srv.URL, AuthProvider: "localProvider", ResponseType: "json"}, 0,
		ui.New(&bytes.Buffer{}, newLineReader(user, "pw-"+user), false, true))
	if err != nil {
		return err
	}
	cfg, err := config.Load(config.FilePath(r.dir))
	if err != nil {
		return err
	}
	s := cfg.Servers[entry]
	if err := config.AdvanceCacheBinding(&s); err != nil {
		return err
	}
	cfg.Servers[entry] = s
	if err := cfg.Write(); err != nil {
		return err
	}
	return r.store.SetToken(config.ServerRef{Name: entry, URL: s.URL}, cred.Token)
}

// logout removes entry's login as auth logout does: a fresh cache binding
// id, then the credential deleted.
func (r *recordFixture) logout(entry string) {
	r.t.Helper()
	var url string
	r.edit(func(cfg *config.Config) {
		s := cfg.Servers[entry]
		url = s.URL
		require.NoError(r.t, config.AdvanceCacheBinding(&s))
		cfg.Servers[entry] = s
	})
	require.NoError(r.t, r.store.DeleteToken(config.ServerRef{Name: entry, URL: url}))
}

// deleteServer removes entry and its credential as config delete-server
// does.
func (r *recordFixture) deleteServer(entry string) {
	r.t.Helper()
	var url string
	r.edit(func(cfg *config.Config) {
		url = cfg.Servers[entry].URL
		delete(cfg.Servers, entry)
		if cfg.CurrentServer == entry {
			cfg.CurrentServer = ""
		}
	})
	require.NoError(r.t, r.store.DeleteToken(config.ServerRef{Name: entry, URL: url}))
}

// moveURL re-addresses entry as config update-server --url does: a fresh
// cache binding id and the new URL, the login removed.
func (r *recordFixture) moveURL(entry, url string) {
	r.t.Helper()
	var was string
	r.edit(func(cfg *config.Config) {
		s := cfg.Servers[entry]
		was = s.URL
		s.URL = url
		require.NoError(r.t, config.AdvanceCacheBinding(&s))
		cfg.Servers[entry] = s
	})
	require.NoError(r.t, r.store.DeleteToken(config.ServerRef{Name: entry, URL: was}))
}

// shimCall runs the legacy `rancher token --user user --cluster c-test`
// against the fixture and returns the account the served token belongs
// to.
func (r *recordFixture) shimCall(user string, terminal bool) (string, error) {
	r.t.Helper()
	p := ui.New(io.Discard, strings.NewReader(""), false, false)
	if terminal {
		p = ui.New(&bytes.Buffer{}, newLineReader("carol", "pw-carol"), false, true)
	}
	cmd := NewLegacyTokenCommand(r.dir, r.store, anchorFor(r.dir)).(runtime.ReadCommand)
	res, err := cmd.Execute(r.t.Context(), tin{s: map[string]string{
		"server": r.srv.URL, "user": user, "cluster": "c-test", "auth-provider": "localProvider",
	}}, p)
	if err != nil {
		return "", err
	}
	return r.owner(res), nil
}

func (r *recordFixture) owner(res runtime.Result) string {
	r.t.Helper()
	cred, ok := res.Data.(*execCredential)
	require.True(r.t, ok)
	owner, found := r.f.BearerOwner(cred.Status.Token)
	require.True(r.t, found, "the served token is one the server issued")
	return strings.TrimPrefix(owner, "user-")
}

// cachedOwner is the account of the token a cache hit for the call serves
// now, through the path the call takes, and whether there was one.
func (r *recordFixture) cachedOwner(name string, shim bool) (string, bool) {
	r.t.Helper()
	var (
		res runtime.Result
		hit bool
	)
	if shim {
		_, _, res, hit, _ = shimProbeFlagCache(r.dir, config.LegacyCachePrefix+name, r.srv.URL, "c-test", anchorFor(r.dir), ui.Discard())
	} else {
		c := tokenCmd{dir: r.dir, store: r.store, anchor: anchorFor(r.dir)}
		_, _, res, hit = c.probeFlagCache(name, r.srv.URL, "c-test", ui.Discard())
	}
	if !hit {
		return "", false
	}
	return r.owner(res), true
}

// recordSetup is a directory state and the call that leaves a record in
// it; every setup but the bare install also holds staging, a user entry
// at the URL with no login.
type recordSetup struct {
	name  string
	call  string
	shim  bool
	build func(r *recordFixture)
	// noRecord is a setup whose call must leave no record a hit serves:
	// the uncached call after it is refused.
	noRecord bool
}

var (
	pluginEntry  = "exec-login-plugin"
	secondPlugin = "exec-login-second"
)

func recordSetups() map[string]recordSetup {
	withStaging := func(build func(r *recordFixture)) func(r *recordFixture) {
		return func(r *recordFixture) {
			r.edit(func(cfg *config.Config) {
				s := config.Server{URL: r.srv.URL}
				require.NoError(r.t, config.AdvanceCacheBinding(&s))
				cfg.Servers["staging"] = s
			})
			build(r)
		}
	}
	current := func(r *recordFixture, entry string) {
		r.edit(func(cfg *config.Config) { cfg.CurrentServer = entry })
	}
	reuse := func(r *recordFixture, on bool) {
		r.edit(func(cfg *config.Config) { cfg.TokenReuse = on })
	}
	noLogin := func(r *recordFixture, entry string) {
		r.edit(func(cfg *config.Config) {
			s := config.Server{URL: r.srv.URL}
			require.NoError(r.t, config.AdvanceCacheBinding(&s))
			cfg.Servers[entry] = s
		})
	}
	out := map[string]recordSetup{}
	for _, s := range []recordSetup{
		{name: "own credential", call: "prod-c-test", build: withStaging(func(r *recordFixture) {
			r.login("prod", "", "alice", false)
			current(r, "prod")
		})},
		{name: "a plugin login entry's own credential", call: pluginEntry + "-c-test", build: withStaging(func(r *recordFixture) {
			reuse(r, true)
			r.login(pluginEntry, "", "svc", true)
		})},
		{name: "a sign-in after its own credential was rejected", call: "prod-c-test", build: withStaging(func(r *recordFixture) {
			noLogin(r, "prod")
			require.NoError(r.t, r.store.SetToken(config.ServerRef{Name: "prod", URL: r.srv.URL}, "rejected-by-the-server"))
			current(r, "prod")
		})},
		{name: "the plugin's login entry answering for it", call: "prod-c-test", build: withStaging(func(r *recordFixture) {
			reuse(r, true)
			noLogin(r, "prod")
			r.login(pluginEntry, "", "svc", true)
		})},
		{name: "the plugin-created login", call: "prod-c-test", build: withStaging(func(r *recordFixture) {
			reuse(r, true)
			noLogin(r, "prod")
		})},
		{name: "the plugin-created login beside another plugin login entry", call: "prod-c-test", noRecord: true, build: withStaging(func(r *recordFixture) {
			// A plugin login entry under another name - one moved here
			// from another URL - holds no login, so the lookup falls back
			// to it, finds nothing, and the plugin-created login adds the
			// URL's second plugin login entry.
			reuse(r, true)
			noLogin(r, "prod")
			r.edit(func(cfg *config.Config) {
				s := config.Server{URL: r.srv.URL, Provenance: clierr.ProvenanceExecLogin}
				require.NoError(r.t, config.AdvanceCacheBinding(&s))
				cfg.Servers[pluginEntry] = s
			})
		})},
		{name: "the plugin-created login for another plugin login entry", call: pluginEntry + "-c-test", build: withStaging(func(r *recordFixture) {
			// The call names a plugin login entry with no login; the
			// plugin-created login stores its sign-in in the URL's own
			// plugin login entry, and the record is bound to the entry the
			// call names, the setting and both plugin login entries.
			reuse(r, true)
			r.edit(func(cfg *config.Config) {
				s := config.Server{URL: r.srv.URL, Provenance: clierr.ProvenanceExecLogin}
				require.NoError(r.t, config.AdvanceCacheBinding(&s))
				cfg.Servers[pluginEntry] = s
			})
		})},
		{name: "an identified call's sign-in", call: "prod-c-test", build: withStaging(func(r *recordFixture) {
			noLogin(r, "prod")
			current(r, "prod")
		})},
		{name: "the ranked winner", call: "kube-c-test", build: withStaging(func(r *recordFixture) {
			r.login("prod", "", "alice", false)
			current(r, "prod")
		})},
		{name: "the ranked winner, no current server", call: "kube-c-test", build: withStaging(func(r *recordFixture) {
			r.login("prod", "", "alice", false)
		})},
		{name: "the ranked winner over another login", call: "kube-c-test", build: withStaging(func(r *recordFixture) {
			r.login("prod", "", "alice", false)
			r.login("staging", "", "bob", false)
			current(r, "prod")
		})},
		{name: "an unnamed sign-in", call: "kube-c-test", build: withStaging(func(r *recordFixture) {
			noLogin(r, "prod")
			current(r, "prod")
		})},
		{name: "the plugin's login entry answering an unnamed call", call: "kube-c-test", build: withStaging(func(r *recordFixture) {
			reuse(r, true)
			r.login(pluginEntry, "", "svc", true)
		})},
		{name: "an unnamed plugin-created login on a bare install", call: "kube-c-test", build: func(r *recordFixture) {
			reuse(r, true)
		}},
		{name: "the legacy shim", call: "alice", shim: true, build: withStaging(func(r *recordFixture) {
			r.login("prod", "", "alice", false)
			current(r, "prod")
		})},
	} {
		out[s.name] = s
	}
	return out
}

// TestCacheRecordAcrossChanges asserts, for each kind of record the exec
// plugin writes and each change that can follow it, whether a cache hit
// still serves the record, and that whenever one does, the token it
// serves belongs to the account the uncached call authenticates as.
// Every setup is shown to serve its record before the change, so a miss
// after it is the change's doing.
func TestCacheRecordAcrossChanges(t *testing.T) {
	type change struct {
		name string
		run  func(r *recordFixture, call string)
	}
	var (
		loginProd    = change{"a login of prod", func(r *recordFixture, _ string) { r.login("prod", "", "bob", false) }}
		logoutProd   = change{"a logout of prod", func(r *recordFixture, _ string) { r.logout("prod") }}
		deleteProd   = change{"config delete-server prod", func(r *recordFixture, _ string) { r.deleteServer("prod") }}
		moveProd     = change{"config update-server --url of prod", func(r *recordFixture, _ string) { r.moveURL("prod", otherServerURL) }}
		loginStaging = change{"a login of staging", func(r *recordFixture, _ string) { r.login("staging", "", "bob", false) }}
		deleteStage  = change{"config delete-server staging", func(r *recordFixture, _ string) { r.deleteServer("staging") }}
		newEntry     = change{"a login creating another entry at the URL", func(r *recordFixture, _ string) { r.login("west", "", "bob", false) }}
		elsewhere    = change{"a login creating an entry at another URL", func(r *recordFixture, _ string) { r.login("elsewhere", otherServerURL, "bob", false) }}
		createProd   = change{"a login creating prod at the URL", func(r *recordFixture, _ string) { r.login("prod", "", "bob", false) }}
		namedHere    = change{"a login creating the entry named like --name at the URL", func(r *recordFixture, call string) { r.login(call, "", "bob", false) }}
		namedThere   = change{"a login creating the entry named like --name at another URL", func(r *recordFixture, call string) { r.login(call, otherServerURL, "bob", false) }}
		useStaging   = change{"config use-server staging", func(r *recordFixture, _ string) {
			r.edit(func(cfg *config.Config) { cfg.CurrentServer = "staging" })
		}}
		reuseOn = change{"config set-token-reuse on", func(r *recordFixture, _ string) {
			r.edit(func(cfg *config.Config) { cfg.TokenReuse = true })
		}}
		reuseOff = change{"config set-token-reuse off", func(r *recordFixture, _ string) {
			r.edit(func(cfg *config.Config) { cfg.TokenReuse = false })
		}}
		loginPlugin  = change{"a login of the plugin's login entry", func(r *recordFixture, _ string) { r.login(pluginEntry, "", "bob", true) }}
		deletePlugin = change{"config delete-server of the plugin's login entry", func(r *recordFixture, _ string) { r.deleteServer(pluginEntry) }}
		noChange     = change{"a config write that changes nothing", func(r *recordFixture, _ string) { r.edit(func(*config.Config) {}) }}
		addPlugin    = change{"a second plugin login entry for the URL", func(r *recordFixture, _ string) { r.login(secondPlugin, "", "bob", true) }}
	)
	rows := []struct {
		setup   string
		change  change
		wantHit bool
	}{
		{"own credential", loginProd, false},
		{"own credential", logoutProd, false},
		{"own credential", deleteProd, false},
		{"own credential", moveProd, false},
		{"own credential", namedHere, false},
		{"own credential", namedThere, false},
		{"own credential", loginStaging, true},
		{"own credential", newEntry, true},
		{"own credential", useStaging, true},
		{"own credential", reuseOn, true},
		{"own credential", addPlugin, true},

		{"a plugin login entry's own credential", reuseOff, false},
		{"a plugin login entry's own credential", loginPlugin, false},
		{"a plugin login entry's own credential", loginStaging, true},
		{"a plugin login entry's own credential", addPlugin, true},
		{"a plugin login entry's own credential", useStaging, true},

		{"a sign-in after its own credential was rejected", loginProd, false},
		{"a sign-in after its own credential was rejected", loginStaging, true},
		{"a sign-in after its own credential was rejected", useStaging, true},

		{"the plugin's login entry answering for it", loginProd, false},
		{"the plugin's login entry answering for it", loginPlugin, false},
		{"the plugin's login entry answering for it", addPlugin, false},
		{"the plugin's login entry answering for it", reuseOff, false},
		{"the plugin's login entry answering for it", loginStaging, true},
		{"the plugin's login entry answering for it", useStaging, true},
		{"the plugin's login entry answering for it", namedHere, false},

		{"the plugin-created login", loginProd, false},
		{"the plugin-created login", addPlugin, false},
		{"the plugin-created login", reuseOff, false},
		{"the plugin-created login", loginStaging, true},
		{"the plugin-created login", useStaging, true},

		{"the plugin-created login beside another plugin login entry", noChange, false},
		{"the plugin-created login beside another plugin login entry", deletePlugin, false},

		{"the plugin-created login for another plugin login entry", noChange, true},
		{"the plugin-created login for another plugin login entry", loginStaging, true},
		{"the plugin-created login for another plugin login entry", loginPlugin, false},
		{"the plugin-created login for another plugin login entry", reuseOff, false},

		{"an identified call's sign-in", loginProd, false},
		{"an identified call's sign-in", reuseOn, false},
		{"an identified call's sign-in", namedThere, false},
		{"an identified call's sign-in", useStaging, true},
		{"an identified call's sign-in", loginStaging, true},
		{"an identified call's sign-in", newEntry, true},
		{"an identified call's sign-in", addPlugin, true},

		{"the ranked winner", loginProd, false},
		{"the ranked winner", loginStaging, false},
		{"the ranked winner", deleteStage, false},
		{"the ranked winner", newEntry, false},
		{"the ranked winner", useStaging, false},
		{"the ranked winner", reuseOn, false},
		{"the ranked winner", namedThere, false},
		{"the ranked winner", elsewhere, true},

		{"the ranked winner, no current server", loginStaging, false},

		{"the ranked winner over another login", useStaging, false},
		{"the ranked winner over another login", elsewhere, true},

		{"an unnamed sign-in", loginStaging, false},
		{"an unnamed sign-in", newEntry, false},
		{"an unnamed sign-in", useStaging, false},
		{"an unnamed sign-in", elsewhere, true},

		{"the plugin's login entry answering an unnamed call", loginStaging, false},
		{"the plugin's login entry answering an unnamed call", reuseOff, false},

		{"an unnamed plugin-created login on a bare install", createProd, false},
		{"an unnamed plugin-created login on a bare install", elsewhere, true},

		{"the legacy shim", loginStaging, false},
		{"the legacy shim", loginProd, false},
		{"the legacy shim", reuseOn, false},
		{"the legacy shim", useStaging, true},
		{"the legacy shim", elsewhere, true},
	}
	setups := recordSetups()
	for _, row := range rows {
		t.Run(row.setup+", then "+row.change.name, func(t *testing.T) {
			s, ok := setups[row.setup]
			require.True(t, ok, "setup %q", row.setup)
			r := newRecordFixture(t, false)
			s.build(r)
			call := func() (string, error) {
				if s.shim {
					return r.shimCall(s.call, true)
				}
				return r.call(s.call, true)
			}
			first, err := call()
			require.NoError(t, err)
			cached, hit := r.cachedOwner(s.call, s.shim)
			if s.noRecord {
				require.False(t, hit, "the setup leaves no record the call serves")
			} else {
				require.True(t, hit, "the setup leaves a record the call serves")
				require.Equal(t, first, cached)
			}

			row.change.run(r, s.call)
			cached, hit = r.cachedOwner(s.call, s.shim)
			assert.Equal(t, row.wantHit, hit, "whether the record is still served")

			require.NoError(t, r.store.DeleteAllCachedCreds())
			uncached, uerr := call()
			if !hit {
				return
			}
			require.NoError(t, uerr, "a record is served only for a call the lookup answers")
			assert.Equal(t, uncached, cached, "a hit serves the account the uncached call authenticates as")
		})
	}
}

// TestPluginCreatedLoginRecordTakesIDsFromItsOwnLookup asserts that the
// record of a plugin-created login made for a call that identifies prod is
// bound to prod's id as the login's lookup read it, under the plugin-login
// lock, before it found prod empty. A login of prod that completes between
// that read and the plugin-created login's own writes leaves the login's
// last write showing prod's new id; a record bound to that id would serve
// the plugin's login where the uncached call now reads prod's.
func TestPluginCreatedLoginRecordTakesIDsFromItsOwnLookup(t *testing.T) {
	var (
		r       *recordFixture
		fired   atomic.Bool
		loginOf = make(chan error, 1)
	)
	r = newRecordFixtureWith(t, true, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			// The plugin-created login's sign-in is its first request after
			// its lookup's read; prod's own login lands there. It runs on
			// the server's goroutine, so it reports rather than fails.
			if req.Method == http.MethodPost && req.URL.Path == "/v1-public/login" && fired.CompareAndSwap(false, true) {
				loginOf <- r.loginAside("prod", "bob")
			}
			next.ServeHTTP(w, req)
		})
	})
	r.edit(func(cfg *config.Config) {
		s := config.Server{URL: r.srv.URL}
		require.NoError(t, config.AdvanceCacheBinding(&s))
		cfg.Servers["prod"] = s
	})

	got, err := r.call("prod-c-test", true)
	require.NoError(t, err)
	require.True(t, fired.Load(), "prod's login landed inside the plugin-created login")
	require.NoError(t, <-loginOf)
	assert.Equal(t, "carol", got, "the plugin-created login's own credential answered this call")

	_, hit := r.cachedOwner("prod-c-test", false)
	assert.False(t, hit, "the record is bound to the id prod had before its login")
	got, err = r.call("prod-c-test", false)
	require.NoError(t, err)
	assert.Equal(t, "bob", got, "the next call reads prod's own login")
}

// TestACacheHitJudgedBeforeUseServerJudgesAgain asserts that a cache hit
// whose config load came before config use-server, and whose confirmation
// comes after it, does not serve the record of a call that identifies no
// entry: the write moves the binding stamp, the hit loads again, and the
// record is bound to the current server the lookup no longer prefers.
func TestACacheHitJudgedBeforeUseServerJudgesAgain(t *testing.T) {
	r := newRecordFixture(t, false)
	r.login("prod", "", "alice", false)
	r.login("staging", "", "bob", false)
	r.edit(func(cfg *config.Config) { cfg.CurrentServer = "prod" })
	got, err := r.call("kube-c-test", false)
	require.NoError(t, err)
	require.Equal(t, "alice", got)

	key := config.CacheKey("kube-c-test", config.CanonicalServerIdentity(r.srv.URL), "c-test")
	switched := false
	_, hit, err := config.CachedHit(anchorFor(r.dir), func(a *config.CacheAnchor) (string, bool, error) {
		tok, _, gerr := r.store.GetCachedCred(key, a)
		if gerr != nil {
			return "", false, nil
		}
		if !switched {
			// After the record was judged against the earlier load, before
			// the stamp is confirmed.
			switched = true
			r.edit(func(cfg *config.Config) { cfg.CurrentServer = "staging" })
		}
		return tok, true, nil
	})
	require.NoError(t, err)
	require.True(t, switched, "the record was judged good against the load before the write")
	assert.False(t, hit, "the hit judges again after the write and misses")

	got, err = r.call("kube-c-test", false)
	require.NoError(t, err)
	assert.Equal(t, "bob", got, "the uncached call prefers the new current server")
}

// TestLegacyShimRecordsNeverIdentifyAnEntry asserts that the legacy
// `rancher token` shim's record is judged as the shim's lookup runs, which
// never identifies an entry: an entry named exactly like the shim's cache
// slot, registered at another URL, leaves the record served, where it
// would refuse a call of the native command with that --name.
func TestLegacyShimRecordsNeverIdentifyAnEntry(t *testing.T) {
	r := newRecordFixture(t, false)
	r.login("prod", "", "alice", false)
	got, err := r.shimCall("alice", false)
	require.NoError(t, err)
	require.Equal(t, "alice", got)

	r.edit(func(cfg *config.Config) {
		s := config.Server{URL: otherServerURL}
		require.NoError(t, config.AdvanceCacheBinding(&s))
		cfg.Servers[config.LegacyCachePrefix+"alice"] = s
	})
	cached, hit := r.cachedOwner("alice", true)
	assert.True(t, hit, "the shim's record is not judged by an identification the shim never makes")
	assert.Equal(t, "alice", cached)
}

// TestAPluginCreatedLoginBesideAnotherPluginEntryLeavesNoRecord asserts
// that a call identifying a user entry with no login, on a URL whose one
// plugin login entry holds no login either, gets its token from the
// plugin-created login and caches nothing: that login adds the URL's
// second plugin login entry, and the lookup of the same call refuses a URL
// with more than one, so a record would be served where the uncached call
// is refused.
func TestAPluginCreatedLoginBesideAnotherPluginEntryLeavesNoRecord(t *testing.T) {
	r := newRecordFixture(t, true)
	for _, e := range []struct {
		name   string
		plugin bool
	}{{"prod", false}, {pluginEntry, true}} {
		r.edit(func(cfg *config.Config) {
			s := config.Server{URL: r.srv.URL}
			if e.plugin {
				s.Provenance = clierr.ProvenanceExecLogin
			}
			require.NoError(t, config.AdvanceCacheBinding(&s))
			cfg.Servers[e.name] = s
		})
	}

	var notices bytes.Buffer
	got, err := r.callTo("prod-c-test", true, &notices)
	require.NoError(t, err)
	assert.Equal(t, "carol", got, "the plugin-created login answers this call")
	assert.NotContains(t, notices.String(), "cach", "the skipped write says nothing; the next call's refusal does")
	cfg, err := config.Load(config.FilePath(r.dir))
	require.NoError(t, err)
	require.Len(t, cfg.PluginLoginEntries(r.srv.URL), 2, "the login added a second plugin login entry")

	_, hit := r.cachedOwner("prod-c-test", false)
	assert.False(t, hit, "no record is served for a call the lookup now refuses")
	_, err = r.call("prod-c-test", false)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want the lookup's refusal, got %v", err)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
}

// TestACredentialThatCannotBeCachedSaysSoInPlainWords asserts the notice
// a call gives when its credential cannot be cached for a reason that is
// not its lookup's refusal: config.yaml could not be read, so nothing can
// say later whom the record belongs to. The call still returns the
// credential it signed in for.
func TestACredentialThatCannotBeCachedSaysSoInPlainWords(t *testing.T) {
	r := newRecordFixture(t, false)
	require.NoError(t, os.WriteFile(config.FilePath(r.dir), []byte("\t: not yaml ["), 0o600))
	var notices bytes.Buffer
	p := ui.New(&notices, newLineReader("carol", "pw-carol"), false, true)
	// --cacert keeps the connection's own read of config.yaml out of the
	// way, so the call reaches the sign-in and the write.
	cmd := NewTokenCommand(r.dir, r.store, anchorFor(r.dir)).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(), tin{s: map[string]string{
		"server": r.srv.URL, "cluster": "c-test", "name": "prod-c-test", "auth-provider": "localProvider",
		"cacert": selfSignedCAPEM(t),
	}}, p)
	require.NoError(t, err)
	assert.Equal(t, "carol", r.owner(res))
	assert.Contains(t, notices.String(),
		"cache write failed: the config directory could not be resolved or its config.yaml could not be read when the credential was made")
	assert.NotContains(t, notices.String(), "binding")
	assert.NotContains(t, notices.String(), "config load")
}

// TestAPauseOfAnotherBoundEntryIsNamedAndResumed asserts the pause rule
// for an entry a record is bound to other than the one the call
// identified: the write is refused, and the exec plugin resumes that
// entry's cache writes when the config lock is free, or names it in its
// notice when a change still holds the lock. Two records: one of a call
// that identifies no entry, with staging paused; one of a call answered by
// the plugin's login entry, with that entry paused.
func TestAPauseOfAnotherBoundEntryIsNamedAndResumed(t *testing.T) {
	cases := []struct {
		name   string
		call   string
		reuse  bool
		paused string
		build  func(r *recordFixture)
	}{
		{name: "a call that identifies no entry", call: "kube-c-test", paused: "staging", build: func(r *recordFixture) {
			r.login("prod", "", "alice", false)
			r.edit(func(cfg *config.Config) {
				s := config.Server{URL: r.srv.URL}
				require.NoError(r.t, config.AdvanceCacheBinding(&s))
				cfg.Servers["staging"] = s
				cfg.CurrentServer = "prod"
			})
		}},
		{name: "a call answered by the plugin's login entry", call: "prod-c-test", reuse: true, paused: pluginEntry, build: func(r *recordFixture) {
			r.edit(func(cfg *config.Config) {
				s := config.Server{URL: r.srv.URL}
				require.NoError(r.t, config.AdvanceCacheBinding(&s))
				cfg.Servers["prod"] = s
			})
			r.login(pluginEntry, "", "svc", true)
		}},
	}
	for _, tc := range cases {
		for _, held := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, lock held %v", tc.name, held), func(t *testing.T) {
				t.Setenv(config.EnvConfigNoLock, "")
				r := newRecordFixture(t, tc.reuse)
				tc.build(r)
				r.edit(func(cfg *config.Config) {
					s := cfg.Servers[tc.paused]
					s.CacheWritesPaused = config.PauseLocked
					cfg.Servers[tc.paused] = s
				})
				if held {
					other := flock.New(config.LockPath(r.dir))
					ok, err := other.TryLock()
					require.NoError(t, err)
					require.True(t, ok)
					t.Cleanup(func() { _ = other.Unlock() })
				}

				var notices bytes.Buffer
				_, err := r.callTo(tc.call, false, &notices)
				require.NoError(t, err, "the credential is returned")
				_, hit := r.cachedOwner(tc.call, false)
				assert.False(t, hit, "no record is written while an entry it is bound to is paused")
				cfg, err := config.Load(config.FilePath(r.dir))
				require.NoError(t, err)
				if held {
					assert.Contains(t, notices.String(), "not caching: a credential change for "+tc.paused+" has not finished")
					assert.True(t, cfg.Servers[tc.paused].Paused(), "the pause stays while the lock is held")
					return
				}
				assert.NotContains(t, notices.String(), "not caching")
				assert.False(t, cfg.Servers[tc.paused].Paused(), "the call resumed the paused entry's cache writes")
			})
		}
	}
}

// TestASignInForAnotherPluginLoginEntryIsCachedForTheEntryItNames
// asserts the record a call naming a plugin login entry with no login gets
// when the plugin-created login stores its sign-in in the URL's own plugin
// login entry: bound to the entry the call names, the token-reuse setting
// and the URL's plugin login entries, each with its id, so the next call
// is served from the cache without signing in again.
func TestASignInForAnotherPluginLoginEntryIsCachedForTheEntryItNames(t *testing.T) {
	r := newRecordFixture(t, true)
	r.edit(func(cfg *config.Config) {
		s := config.Server{URL: r.srv.URL, Provenance: clierr.ProvenanceExecLogin}
		require.NoError(t, config.AdvanceCacheBinding(&s))
		cfg.Servers[pluginEntry] = s
	})
	name := pluginEntry + "-c-test"
	got, err := r.call(name, true)
	require.NoError(t, err)
	assert.Equal(t, "carol", got)

	cfg, err := config.Load(config.FilePath(r.dir))
	require.NoError(t, err)
	plugins := cfg.PluginLoginEntries(r.srv.URL)
	require.Len(t, plugins, 2, "the sign-in went to the URL's own plugin login entry")
	raw, err := os.ReadFile(filepath.Join(r.dir, "token-cache.yaml"))
	require.NoError(t, err)
	var records map[string]diskRecord
	require.NoError(t, yaml.Unmarshal(raw, &records))
	key := config.CacheKey(name, config.CanonicalServerIdentity(r.srv.URL), "c-test")
	rec, ok := records[key]
	require.True(t, ok, "the record is written")
	listed := make([]diskEntry, len(plugins))
	for i, n := range plugins {
		listed[i] = diskEntry{Name: n, BindingID: cfg.Servers[n].CacheBindingID}
	}
	assert.Equal(t, diskRecord{Kind: "identifiedEntry", Identified: pluginEntry, Entry: pluginEntry,
		BindingID: cfg.Servers[pluginEntry].CacheBindingID, Entries: &listed, TokenReuse: new(true)}, rec)

	got, err = r.call(name, false)
	require.NoError(t, err, "the next call, with no terminal, is served from the cache")
	assert.Equal(t, "carol", got)
}
