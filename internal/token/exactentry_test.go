package token

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

const otherServerURL = "https://other.example"

// rowEntry is one server entry a row seeds. url is the fixture server when
// empty. user names the account whose login credential the entry stores;
// empty stores none.
type rowEntry struct {
	name    string
	url     string
	user    string
	plugin  bool
	current bool
	// unreadable makes the store fail every read of the entry's credential,
	// though it holds one.
	unreadable bool
	// ca is the CA the entry records: "server" is the fixture's own
	// certificate, "wrong" a certificate that signed nothing.
	ca string
}

// entryRow is one state of the table: the directory, the setting, the
// call and what the call must do.
type entryRow struct {
	name     string
	entries  []rowEntry
	reuse    bool
	flagName string
	cluster  string
	shim     bool
	terminal bool
	// noServer leaves --server off, so the server and its CA come from the
	// current entry.
	noServer bool
	// tls serves the fixture over TLS, so the CA an entry records decides
	// whether the call can connect.
	tls bool

	// wantUser is the account the served credential belongs to.
	wantUser string
	// wantCode and wantMsg describe the typed error instead; wantMsg
	// entries all appear in the message.
	wantCode string
	wantMsg  []string
	// wantNotMsg entries appear in no part of the message.
	wantNotMsg []string
	// noNetwork asserts no login was started and no token was created.
	noNetwork bool
	// wantPluginEntry asserts the call left the plugin's login entry.
	wantPluginEntry bool
	// thenNoTerminal repeats the call after the cache is cleared, with no
	// terminal, and expects the same account.
	thenNoTerminal bool
	// mutation names the change to the production code that makes this
	// row fail.
	mutation string
}

func entryTableUsers() []testrancher.User {
	var users []testrancher.User
	for _, u := range []string{"alice", "bob", "svc", "carol"} {
		users = append(users, testrancher.User{Name: u, Password: "pw-" + u, UserID: "user-" + u})
	}
	return users
}

func acquireFor(t *testing.T, srv *httptest.Server, user string) string {
	t.Helper()
	cred, err := AcquireLoginCredential(t.Context(), srv.Client(),
		LoginRequest{Server: srv.URL, AuthProvider: "localProvider", ResponseType: "json"}, 0,
		ui.New(&bytes.Buffer{}, newLineReader(user, "pw-"+user), false, true))
	require.NoError(t, err)
	return cred.Token
}

func (r entryRow) run(t *testing.T) {
	keyring.MockInit()
	t.Setenv(execInfoEnv, "")
	t.Setenv(config.EnvCredentialStore, "")
	var (
		f   *testrancher.Facade
		srv *httptest.Server
	)
	if r.tls {
		opts := testrancher.Options{Users: entryTableUsers()}
		f = newFacadeWithClusters(&opts)
		srv = httptest.NewTLSServer(f)
		t.Cleanup(srv.Close)
	} else {
		f, srv = newTokenFacade(t, testrancher.Options{Users: entryTableUsers()})
	}
	wrongCA := selfSignedCAPEM(t)

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{Servers: map[string]config.Server{}, TokenReuse: r.reuse, CredentialStore: "file"}
	cfg.SetPath(config.FilePath(dir))
	for _, e := range r.entries {
		url := e.url
		if url == "" {
			url = srv.URL
		}
		s := config.Server{URL: url}
		switch e.ca {
		case "server":
			s.CA = serverCAPEM(srv)
		case "wrong":
			s.CA = wrongCA
		}
		if e.plugin {
			s.Provenance = clierr.ProvenanceExecLogin
		}
		cfg.Servers[e.name] = s
		if e.current {
			cfg.CurrentServer = e.name
		}
		if e.user != "" {
			require.NoError(t, store.SetToken(config.ServerRef{Name: e.name, URL: url}, acquireFor(t, srv, e.user)))
		}
	}
	require.NoError(t, cfg.Write())

	cluster := r.cluster
	if cluster == "" {
		cluster = "c-test"
	}
	cmdStore := config.CredStore(store)
	for _, e := range r.entries {
		if e.unreadable {
			cmdStore = entryErrStore{CredStore: store, entry: e.name, err: clierr.New(clierr.CodeGeneric, "credential store unreachable: no session bus")}
		}
	}
	prompter := func(terminal bool) runtime.Prompter {
		if terminal {
			return ui.New(&bytes.Buffer{}, newLineReader("carol", "pw-carol"), false, true)
		}
		return ui.New(io.Discard, strings.NewReader(""), false, false)
	}
	call := func(terminal bool) (runtime.Result, error) {
		if r.shim {
			cmd := NewLegacyTokenCommand(dir, cmdStore, anchorFor(dir)).(runtime.ReadCommand)
			return cmd.Execute(t.Context(), tin{s: map[string]string{
				"server": srv.URL, "user": r.flagName, "cluster": cluster, "auth-provider": "localProvider",
			}}, prompter(terminal))
		}
		cmd := NewTokenCommand(dir, cmdStore, anchorFor(dir)).(runtime.ReadCommand)
		flags := map[string]string{"server": srv.URL, "cluster": cluster, "name": r.flagName, "auth-provider": "localProvider"}
		if r.noServer {
			delete(flags, "server")
		}
		return cmd.Execute(t.Context(), tin{s: flags}, prompter(terminal))
	}
	served := func(res runtime.Result) string {
		cred, ok := res.Data.(*execCredential)
		require.True(t, ok)
		owner, found := f.BearerOwner(cred.Status.Token)
		require.True(t, found, "the served token is one the server issued")
		return strings.TrimPrefix(owner, "user-")
	}

	logins, creates := f.LoginCount(), f.DerivedCreateRequests()
	res, err := call(r.terminal)
	if r.wantCode != "" {
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "want a typed error, got %v", err)
		assert.Equal(t, r.wantCode, ce.Code)
		for _, m := range r.wantMsg {
			assert.Contains(t, ce.Message, strings.ReplaceAll(m, "%URL%", srv.URL))
		}
		for _, m := range r.wantNotMsg {
			assert.NotContains(t, ce.Message, m)
		}
	} else {
		require.NoError(t, err)
		assert.Equal(t, r.wantUser, served(res))
	}
	if r.noNetwork {
		assert.Equal(t, logins, f.LoginCount(), "no login was started")
		assert.Equal(t, creates, f.DerivedCreateRequests(), "no token was created")
	}
	if r.wantPluginEntry {
		reloaded, lerr := config.Load(config.FilePath(dir))
		require.NoError(t, lerr)
		s, ok := reloaded.Servers[config.ExecLoginEntryName(srv.URL)]
		require.True(t, ok, "the plugin's login entry was written")
		assert.Equal(t, clierr.ProvenanceExecLogin, s.Provenance)
	}
	if r.thenNoTerminal {
		require.NoError(t, store.DeleteAllCachedCreds())
		res, err = call(false)
		require.NoError(t, err)
		assert.Equal(t, r.wantUser, served(res), "the next run finds the login the first run left")
	}
}

// TestExecPluginEntryStateTable asserts, for every state of the directory,
// the setting and the exec entry's name, which entry's credential the exec
// plugin authenticates with, or the typed error it stops with. Each row
// names the change to the production code that makes it fail.
func TestExecPluginEntryStateTable(t *testing.T) {
	prod := rowEntry{name: "prod", user: "alice"}
	prodEU := rowEntry{name: "prod-eu", user: "bob"}
	prodEUNoCred := rowEntry{name: "prod-eu"}
	seed := rowEntry{name: "exec-login-seed", user: "svc", plugin: true}
	seedTwo := rowEntry{name: "exec-login-seed-2", user: "svc", plugin: true}
	const (
		useServerUnset = clierr.Rancher + " config use-server <entry> (this sets the directory's current server, which is unset now), then " + clierr.Rancher + " kubeconfig --cluster 'c-test'"
		useServerBack  = clierr.Rancher + " config use-server <entry> (this switches the directory's current server; switch back with " + clierr.Rancher + " config use-server 'staging'), then " + clierr.Rancher + " kubeconfig --cluster 'c-test'"
	)

	rows := []entryRow{
		{
			name:     "entry prod-eu named by entry-cluster uses its own credential, not prod's",
			entries:  []rowEntry{prod, prodEU},
			flagName: "prod-eu-c-test",
			wantUser: "bob",
			mutation: "identify by prefix again: a candidate whose name is a prefix of --name ranks ahead of the rest",
		},
		{
			name:     "entry prod named by entry-cluster uses prod",
			entries:  []rowEntry{prod, prodEU},
			flagName: "prod-c-test",
			wantUser: "alice",
			mutation: "drop the entry-cluster step",
		},
		{
			name:     "an exact entry name uses its own credential",
			entries:  []rowEntry{prod, prodEU},
			flagName: "prod-eu",
			wantUser: "bob",
			mutation: "drop the exact-name step",
		},
		{
			name:     "an exact entry name wins over the entry-cluster form",
			entries:  []rowEntry{prod, prodEU, {name: "prod-eu-c-test"}},
			flagName: "prod-eu-c-test", terminal: true,
			wantUser: "carol",
			mutation: "try the entry-cluster form before the exact name",
		},
		{
			name:     "an identified entry without a credential never falls back to another entry: sign-in with a terminal",
			entries:  []rowEntry{prod, prodEUNoCred},
			flagName: "prod-eu-c-test", terminal: true,
			wantUser: "carol",
			mutation: "read every entry on the URL when the identified entry has no credential",
		},
		{
			name:     "an identified entry without a credential never falls back to another entry: no terminal",
			entries:  []rowEntry{prod, prodEUNoCred},
			flagName: "prod-eu-c-test",
			wantCode: clierr.CodeAuth,
			wantMsg:  []string{"interactive login required", clierr.Rancher + " auth login 'prod-eu' --url", "--token-stdin"},
			// prod is not the current server, and its login is not the one
			// the exec entry names, so create kubeconfig is not offered.
			wantNotMsg: []string{"create kubeconfig"},
			noNetwork:  true,
			mutation:   "read every entry on the URL when the identified entry has no credential; suggest the --name as the entry to sign in to",
		},
		{
			name:     "the plugin's login entry answers for an identified entry without a credential while token reuse is on",
			entries:  []rowEntry{prod, prodEUNoCred, seed},
			reuse:    true,
			flagName: "prod-eu-c-test",
			wantUser: "svc",
			mutation: "drop the plugin login entry fallback",
		},
		{
			name:     "the plugin's login entry is not read with token reuse off",
			entries:  []rowEntry{prod, prodEUNoCred, seed},
			flagName: "prod-eu-c-test", terminal: true,
			wantUser: "carol",
			mutation: "allow the plugin login entry fallback with token reuse off",
		},
		{
			name:     "an identified entry's own credential is preferred to the plugin's login entry",
			entries:  []rowEntry{prod, prodEU, seed},
			reuse:    true,
			flagName: "prod-eu-c-test",
			wantUser: "bob",
			mutation: "read the plugin login entry before the identified entry",
		},
		{
			name:     "a sign-in after a miss leaves the plugin's login entry, which the next run finds",
			entries:  []rowEntry{prod, prodEUNoCred},
			reuse:    true,
			flagName: "prod-eu-c-test", terminal: true,
			wantUser: "carol", wantPluginEntry: true, thenNoTerminal: true,
			mutation: "drop the plugin login entry fallback",
		},
		{
			name:     "a bare install signs in and leaves the plugin's login entry, which the next run finds",
			reuse:    true,
			flagName: "prod-eu-c-test", terminal: true,
			wantUser: "carol", wantPluginEntry: true, thenNoTerminal: true,
			mutation: "drop the plugin login entry fallback",
		},
		{
			name:     "a bare install with no terminal names the remedies",
			flagName: "prod-eu-c-test",
			wantCode: clierr.CodeAuth,
			wantMsg:  []string{"interactive login required", clierr.Rancher + " auth login", "--token-stdin"},
			// No login anywhere, so create kubeconfig could not run.
			wantNotMsg: []string{"create kubeconfig"},
			noNetwork:  true,
			mutation:   "none: the bare install is unchanged and the row asserts it stays so",
		},
		{
			name:     "an identified plugin login entry is not read with token reuse off",
			entries:  []rowEntry{prod, seed},
			flagName: "exec-login-seed", terminal: true,
			wantUser: "carol",
			mutation: "read an identified plugin login entry whatever the setting",
		},
		{
			name:      "more than one plugin login entry for the URL is an error, not a choice",
			entries:   []rowEntry{prod, prodEUNoCred, seed, seedTwo},
			reuse:     true,
			flagName:  "prod-eu-c-test",
			wantCode:  clierr.CodeAmbiguousSelection,
			wantMsg:   []string{"'exec-login-seed'", "'exec-login-seed-2'", clierr.Rancher + " auth logout"},
			noNetwork: true,
			mutation:  "use the first plugin login entry",
		},
		{
			name:      "an exact name registered at another URL is an error, not a use of this URL's entries",
			entries:   []rowEntry{prod, {name: "prod-eu", url: otherServerURL, user: "bob"}},
			flagName:  "prod-eu",
			wantCode:  clierr.CodeConflict,
			wantMsg:   []string{"'prod-eu'", otherServerURL, "runs for %URL%", clierr.Rancher + " kubeconfig --cluster 'c-test'"},
			noNetwork: true,
			mutation:  "treat an identified entry on another URL as identifying nothing",
		},
		{
			name:      "an entry-cluster name registered at another URL is an error, not a use of this URL's entries",
			entries:   []rowEntry{prod, {name: "prod-eu", url: otherServerURL, user: "bob"}},
			flagName:  "prod-eu-c-test",
			wantCode:  clierr.CodeConflict,
			wantMsg:   []string{"'prod-eu'", otherServerURL, "%URL%"},
			noNetwork: true,
			mutation:  "treat an identified entry on another URL as identifying nothing",
		},
		{
			name:     "a call naming no entry uses the current server's credential",
			entries:  []rowEntry{prod, {name: "prod-eu", user: "bob", current: true}},
			wantUser: "bob",
			mutation: "drop the current-server preference",
		},
		{
			name:     "a call naming no entry uses the one entry that has a credential",
			entries:  []rowEntry{prod, prodEUNoCred},
			wantUser: "alice",
			mutation: "none: unchanged; asserts the single candidate case stays",
		},
		{
			name:     "a call naming no entry prefers a user entry to the plugin's login entry",
			entries:  []rowEntry{prod, seed},
			reuse:    true,
			wantUser: "alice",
			mutation: "drop the provenance tier",
		},
		{
			name:     "a name that identifies no entry uses the one entry that has a credential",
			entries:  []rowEntry{prod},
			flagName: "kube-c-test",
			wantUser: "alice",
			mutation: "none: unchanged; asserts the single candidate case stays",
		},
		{
			name:      "a name that identifies no entry, with several entries that have a credential, is an error naming them",
			entries:   []rowEntry{prod, prodEU},
			flagName:  "kube-c-test",
			wantCode:  clierr.CodeAmbiguousSelection,
			wantMsg:   []string{"'prod', 'prod-eu'", "give the exec entry --name <entry>-c-test", useServerUnset},
			noNetwork: true,
			mutation:  "pick the first entry at the winning rank",
		},
		{
			name:      "no name, with several entries that have a credential, is an error naming them",
			entries:   []rowEntry{prod, prodEU},
			wantCode:  clierr.CodeAmbiguousSelection,
			wantMsg:   []string{"'prod', 'prod-eu'", "give the exec entry --name <entry>-c-test", useServerUnset},
			noNetwork: true,
			mutation:  "pick the first entry at the winning rank",
		},
		{
			name:      "an entry-cluster name for another cluster identifies nothing",
			entries:   []rowEntry{prod, prodEU},
			flagName:  "prod-eu-c-other",
			wantCode:  clierr.CodeAmbiguousSelection,
			wantMsg:   []string{"'prod', 'prod-eu'"},
			noNetwork: true,
			mutation:  "match the entry-cluster form by prefix instead of by the call's own cluster",
		},
		{
			name:      "several plugin login entries with a credential, and no user entry, is an error naming them",
			entries:   []rowEntry{seed, seedTwo},
			reuse:     true,
			wantCode:  clierr.CodeAmbiguousSelection,
			wantMsg:   []string{"'exec-login-seed', 'exec-login-seed-2'", clierr.Rancher + " auth logout <entry>", clierr.Rancher + " config delete-server <entry>"},
			noNetwork: true,
			mutation:  "pick the first entry at the winning rank; give the regenerate remedy for a tie of plugin login entries",
		},
		{
			name:    "the shim with several entries that have a credential is an error naming them and the remedy",
			entries: []rowEntry{prod, prodEU},
			shim:    true, flagName: "alice",
			wantCode:  clierr.CodeAmbiguousSelection,
			wantMsg:   []string{"'prod', 'prod-eu'", "legacy rancher token command", useServerUnset},
			noNetwork: true,
			mutation:  "pick the first entry at the winning rank",
		},
		{
			name:    "the shim with a user called like an entry is still a tie",
			entries: []rowEntry{prod, prodEU},
			shim:    true, flagName: "prod-eu",
			wantCode:  clierr.CodeAmbiguousSelection,
			wantMsg:   []string{"'prod', 'prod-eu'"},
			noNetwork: true,
			mutation:  "pick the first entry at the winning rank",
		},
		{
			name:    "the shim never identifies an entry: one named like its cache slot does not win",
			entries: []rowEntry{{name: "legacy:alice", user: "bob"}, prod},
			shim:    true, flagName: "alice",
			wantCode:  clierr.CodeAmbiguousSelection,
			wantMsg:   []string{"'legacy:alice', 'prod'"},
			noNetwork: true,
			mutation:  "let the shim identify an entry by its cache-slot name",
		},
		{
			name:    "the shim uses the one entry that has a credential",
			entries: []rowEntry{prod, prodEUNoCred},
			shim:    true, flagName: "alice",
			wantUser: "alice",
			mutation: "none: unchanged; asserts the single candidate case stays",
		},
		{
			name:    "the shim prefers a user entry to the plugin's login entry",
			entries: []rowEntry{prod, seed},
			reuse:   true,
			shim:    true, flagName: "alice",
			wantUser: "alice",
			mutation: "drop the provenance tier",
		},
		{
			name:     "an identified entry's CA is used, not the current server's",
			entries:  []rowEntry{{name: "prod", user: "alice", current: true, ca: "wrong"}, {name: "prod-eu", user: "bob", ca: "server"}},
			tls:      true,
			flagName: "prod-eu-c-test",
			wantUser: "bob",
			mutation: "choose the CA of the current server or the first entry by name, whatever the call names",
		},
		{
			name:      "an identified entry's CA is used even when another entry on the URL has one that would connect",
			entries:   []rowEntry{{name: "prod", user: "alice", current: true, ca: "server"}, {name: "prod-eu", user: "bob", ca: "wrong"}},
			tls:       true,
			flagName:  "prod-eu-c-test",
			wantCode:  clierr.CodeGeneric,
			wantMsg:   []string{"certificate"},
			noNetwork: true,
			mutation:  "choose the CA of the current server or the first entry by name, whatever the call names",
		},
		{
			name:     "a call naming no entry uses the current server's CA",
			entries:  []rowEntry{{name: "prod", user: "alice", current: true, ca: "server"}, {name: "prod-eu", user: "bob", ca: "wrong"}},
			tls:      true,
			flagName: "kube-c-test",
			wantUser: "alice",
			mutation: "none: unchanged; asserts the CA choice for an unidentified call stays",
		},
		{
			name:      "a tie with a current server that has no login names how to switch back",
			entries:   []rowEntry{prod, prodEU, {name: "staging", current: true}},
			flagName:  "kube-c-test",
			wantCode:  clierr.CodeAmbiguousSelection,
			wantMsg:   []string{"'prod', 'prod-eu'", useServerBack},
			noNetwork: true,
			mutation:  "leave out the switch-back clause",
		},
		{
			name:      "with no --server, an identified entry's CA is used: its CA does not connect, the current server's would",
			entries:   []rowEntry{{name: "prod", user: "alice", current: true, ca: "server"}, {name: "prod-eu", user: "bob", ca: "wrong"}},
			tls:       true,
			noServer:  true,
			flagName:  "prod-eu-c-test",
			wantCode:  clierr.CodeGeneric,
			wantMsg:   []string{"certificate"},
			noNetwork: true,
			mutation:  "take the current entry's CA when --server is absent, whatever the call names",
		},
		{
			name:     "with no --server, an identified entry's CA is used: it connects, the current server's would not",
			entries:  []rowEntry{{name: "prod", user: "alice", current: true, ca: "wrong"}, {name: "prod-eu", user: "bob", ca: "server"}},
			tls:      true,
			noServer: true,
			flagName: "prod-eu-c-test",
			wantUser: "bob",
			mutation: "take the current entry's CA when --server is absent, whatever the call names",
		},
		{
			name:      "with no --server, an entry registered at another URL is an error",
			entries:   []rowEntry{{name: "prod", user: "alice", current: true}, {name: "prod-eu", url: otherServerURL, user: "bob"}},
			noServer:  true,
			flagName:  "prod-eu-c-test",
			wantCode:  clierr.CodeConflict,
			wantMsg:   []string{"'prod-eu'", otherServerURL, "runs for %URL%"},
			noNetwork: true,
			mutation:  "identify only when --server is given",
		},
		{
			name:     "with no --server and no entry identified, the current server's CA is used",
			entries:  []rowEntry{{name: "prod", user: "alice", current: true, ca: "server"}, {name: "prod-eu", ca: "wrong"}},
			tls:      true,
			noServer: true,
			flagName: "kube-c-test",
			wantUser: "alice",
			mutation: "none: unchanged; asserts the CA choice stays",
		},
		{
			name:      "an unreadable entry at the winning rank is an error, not a pick of the readable one: no current server",
			entries:   []rowEntry{{name: "prod", user: "alice", unreadable: true}, prodEU},
			flagName:  "kube-c-test",
			wantCode:  clierr.CodeGeneric,
			wantMsg:   []string{"credential store unreachable"},
			noNetwork: true,
			mutation:  "skip an entry whose credential cannot be read and choose among the rest",
		},
		{
			name:      "an unreadable current server is an error, not a pick of the readable entry",
			entries:   []rowEntry{{name: "prod", user: "alice", current: true, unreadable: true}, prodEU},
			flagName:  "kube-c-test",
			wantCode:  clierr.CodeGeneric,
			wantMsg:   []string{"credential store unreachable"},
			noNetwork: true,
			mutation:  "skip an entry whose credential cannot be read and choose among the rest",
		},
		{
			name:     "an unreadable entry below a clear winner does not block it",
			entries:  []rowEntry{{name: "prod", user: "alice", current: true}, {name: "prod-eu", user: "bob", unreadable: true}},
			flagName: "kube-c-test",
			wantUser: "alice",
			mutation: "return a read failure whatever its rank",
		},
		{
			name:     "an unreadable plugin login entry below a user entry does not block it",
			entries:  []rowEntry{prod, {name: "exec-login-seed", user: "svc", plugin: true, unreadable: true}},
			reuse:    true,
			flagName: "kube-c-test",
			wantUser: "alice",
			mutation: "return a read failure whatever its rank",
		},
		{
			name:      "the shim ignores the current server: two entries that have a credential, one current, is still a tie",
			entries:   []rowEntry{{name: "prod", user: "alice", current: true}, prodEU},
			shim:      true,
			flagName:  "alice",
			wantCode:  clierr.CodeAmbiguousSelection,
			wantMsg:   []string{"'prod', 'prod-eu'", "legacy rancher token command"},
			noNetwork: true,
			mutation:  "let the shim's ranking honour the directory's current server",
		},
		{
			name:       "an entry moved to another URL, now the current server, with no entry left at the call's URL: no switch, a login, then the kubeconfig",
			entries:    []rowEntry{{name: "prod-eu", url: otherServerURL, user: "bob", current: true}},
			flagName:   "prod-eu-c-test",
			wantCode:   clierr.CodeConflict,
			wantMsg:    []string{"no entry is registered there", "regenerate the kubeconfig from 'prod-eu' with " + clierr.Rancher + " auth login 'prod-eu' (if it has no login), then " + clierr.Rancher + " kubeconfig --cluster 'c-test'"},
			wantNotMsg: []string{"use-server", "switch back"},
			noNetwork:  true,
			mutation:   "offer the switch to the entry that is already the current server; leave out the login step",
		},
		{
			name:      "an entry moved to another URL, not the current server: switch, login, then the kubeconfig",
			entries:   []rowEntry{{name: "prod-eu", url: otherServerURL, user: "bob"}, {name: "staging", url: "https://third.example", current: true}},
			flagName:  "prod-eu-c-test",
			wantCode:  clierr.CodeConflict,
			wantMsg:   []string{"no entry is registered there", clierr.Rancher + " config use-server 'prod-eu' (this switches the directory's current server; switch back with " + clierr.Rancher + " config use-server 'staging'), then " + clierr.Rancher + " auth login 'prod-eu' (if it has no login), then " + clierr.Rancher + " kubeconfig --cluster 'c-test'"},
			noNetwork: true,
			mutation:  "leave out the switch for an entry that is not the current server",
		},
		{
			name:      "an entry at another URL with an entry at the call's URL: both routes are offered",
			entries:   []rowEntry{prod, {name: "prod-eu", url: otherServerURL, user: "bob"}},
			flagName:  "prod-eu-c-test",
			wantCode:  clierr.CodeConflict,
			wantMsg:   []string{"to use 'prod-eu', regenerate the kubeconfig from it with " + clierr.Rancher + " config use-server 'prod-eu'", "regenerate it from an entry registered there with " + clierr.Rancher + " config use-server <entry>"},
			noNetwork: true,
			mutation:  "offer only the route through an entry at the call's URL",
		},
		{
			name:     "with no --server, an unreadable current entry does not stop a call that names another entry",
			entries:  []rowEntry{{name: "prod", user: "alice", current: true, unreadable: true, ca: "server"}, {name: "prod-eu", user: "bob", ca: "server"}},
			tls:      true,
			noServer: true,
			flagName: "prod-eu-c-test",
			wantUser: "bob",
			mutation: "read the current entry's credential to find where to connect",
		},
	}
	for _, r := range rows {
		t.Run(r.name, r.run)
	}
}

// lookupStored is the credential lookup as storedCredToken runs it: the
// name is identified once, then the credential is read for that answer.
func lookupStored(cfg *config.Config, store config.CredStore, server, wantName, clusterID string, shim bool) (storedCredential, bool, error) {
	l := lookupStoredLogin(cfg, store, server, wantName, clusterID, shim, entryChoice{}, userCheck{})
	return l.cred, l.Outcome == LookupAnswered, l.Err
}

// TestIdentifyEntry asserts what the lookup makes of config.IdentifyEntry's
// answer (its rules are asserted in internal/config): an identified entry
// is passed through, none is none, and an entry registered at another URL
// is the typed refusal naming the entry, never a reason to use the URL's
// other entries.
func TestIdentifyEntry(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Servers: map[string]config.Server{
		"prod":       {URL: "https://r.ex"},
		"prod-elsew": {URL: "https://elsewhere.ex"},
	}}
	got, err := identifyEntry(cfg, "https://r.ex", "prod-c-1", "c-1", false)
	require.NoError(t, err)
	assert.Equal(t, "prod", got)

	got, err = identifyEntry(cfg, "https://r.ex", "kube-c-1", "c-1", false)
	require.NoError(t, err)
	assert.Empty(t, got)

	for _, name := range []string{"prod-elsew", "prod-elsew-c-1"} {
		got, err = identifyEntry(cfg, "https://r.ex", name, "c-1", false)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "%s: %v", name, err)
		assert.Equal(t, clierr.CodeConflict, ce.Code, name)
		assert.Contains(t, ce.Message, "prod-elsew", name)
		assert.Empty(t, got, name)
	}
}

// TestStoredTokenForServerIdentifiedEntryFailuresAreNotMasked asserts that
// any failure reading the identified entry's own credential is returned,
// while another entry on the URL that would answer is never asked: a store
// error, an identity error, and a failure on the plugin's login entry.
func TestStoredTokenForServerIdentifiedEntryFailuresAreNotMasked(t *testing.T) {
	unreachable := errors.New(`keyring get "prod-eu": dbus: no session bus`)
	identity := clierr.New(clierr.CodeCredentialReplaced, "the stored credential for 'prod-eu' was replaced").WithCause(config.ErrCredentialIdentity)
	newCfg := func() *config.Config {
		return &config.Config{TokenReuse: true, Servers: map[string]config.Server{
			"prod":            {URL: "https://r.ex"},
			"prod-eu":         {URL: "https://r.ex"},
			"exec-login-seed": {URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin},
		}}
	}
	cases := []struct {
		name  string
		entry string
		err   error
	}{
		{"a store error on the identified entry", "prod-eu", unreachable},
		{"an identity error on the identified entry", "prod-eu", identity},
		{"a store error on the plugin's login entry, after a miss on the identified entry", "exec-login-seed", unreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := config.NewFileStore(t.TempDir())
			require.NoError(t, base.SetToken(config.ServerRef{Name: "prod"}, "tok-alice"))
			if tc.entry != "prod-eu" {
				require.NoError(t, base.SetToken(config.ServerRef{Name: "exec-login-seed"}, "tok-seed"))
			}
			store := entryErrStore{CredStore: base, entry: tc.entry, err: tc.err}
			got, ok, err := lookupStored(newCfg(), store, "https://r.ex", "prod-eu", "", false)
			require.ErrorIs(t, err, tc.err)
			assert.False(t, ok)
			assert.Empty(t, got.token, "no other entry's credential comes back")
		})
	}
}

// TestCacheRecordIsAnchoredToTheIdentifiedEntry asserts that the record a
// sign-in leaves is bound to the entry the exec entry names, not to another
// entry on the URL: a change to the named entry's login makes the record a
// miss, and a change to the other entry's leaves it served.
func TestCacheRecordIsAnchoredToTheIdentifiedEntry(t *testing.T) {
	keyring.MockInit()
	t.Setenv(execInfoEnv, "")
	t.Setenv(config.EnvCredentialStore, "")
	_, srv := newTokenFacade(t, testrancher.Options{Users: entryTableUsers()})

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{CredentialStore: "file", CurrentServer: "prod", Servers: map[string]config.Server{
		"prod":    {URL: srv.URL, CacheBindingID: "b-prod"},
		"prod-eu": {URL: srv.URL, CacheBindingID: "b-eu"},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: srv.URL}, acquireFor(t, srv, "alice")))

	cmd := NewTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(), tin{s: map[string]string{
		"server": srv.URL, "cluster": "c-test", "name": "prod-eu-c-test", "auth-provider": "localProvider",
	}}, ui.New(&bytes.Buffer{}, newLineReader("carol", "pw-carol"), false, true))
	require.NoError(t, err)

	probe := func() bool {
		c := tokenCmd{dir: dir, store: store, anchor: anchorFor(dir)}
		_, _, _, hit := c.probeFlagCache("prod-eu-c-test", srv.URL, "c-test", "", ui.Discard())
		return hit
	}
	require.True(t, probe(), "the sign-in's record is served")

	loaded, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	other := loaded.Servers["prod"]
	other.CacheBindingID = "b-prod-2"
	loaded.Servers["prod"] = other
	require.NoError(t, loaded.Write())
	assert.True(t, probe(), "another entry's login changing leaves the record served")

	named := loaded.Servers["prod-eu"]
	named.CacheBindingID = "b-eu-2"
	loaded.Servers["prod-eu"] = named
	require.NoError(t, loaded.Write())
	assert.False(t, probe(), "the named entry's login changing makes it a miss")
}

// TestResolveConnCAFollowsTheIdentifiedEntry asserts which CA a call
// connects with: an explicit --cacert first, then the identified entry's
// own CA (none if it has none, never another entry's), else the current
// server's or the first entry by name as before. An identified entry at
// another URL stops the call.
func TestResolveConnCAFollowsTheIdentifiedEntry(t *testing.T) {
	caProd := selfSignedCAPEM(t)
	caEU := selfSignedCAPEM(t)
	caFlag := selfSignedCAPEM(t)
	cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{
		"prod":       {URL: "https://r.ex", CA: caProd},
		"prod-eu":    {URL: "https://r.ex", CA: caEU},
		"prod-plain": {URL: "https://r.ex"},
		"prod-elsew": {URL: "https://elsewhere.ex", CA: caEU},
	}}
	dir := t.TempDir()
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cases := []struct {
		name, wantName, cacert string
		shim, noServer         bool
		want                   string
		wantCode               string
	}{
		{name: "the identified entry's CA, not the current server's", wantName: "prod-eu-c-1", want: caEU},
		{name: "an exact entry name", wantName: "prod-eu", want: caEU},
		{name: "an explicit --cacert wins", wantName: "prod-eu-c-1", cacert: caFlag, want: caFlag},
		{name: "an identified entry with no CA borrows none", wantName: "prod-plain-c-1", want: ""},
		{name: "no name: the current server's CA", want: caProd},
		{name: "a name that identifies nothing: the current server's CA", wantName: "kube-c-1", want: caProd},
		{name: "the shim: first entry by name, whatever its user is called", wantName: "prod-eu", shim: true, want: caProd},
		{name: "an identified entry at another URL", wantName: "prod-elsew-c-1", wantCode: clierr.CodeConflict},
		{name: "no --server: the identified entry's CA, not the current server's", wantName: "prod-eu-c-1", noServer: true, want: caEU},
		{name: "no --server: an explicit --cacert wins", wantName: "prod-eu-c-1", cacert: caFlag, noServer: true, want: caFlag},
		{name: "no --server, nothing identified: the current server's CA", wantName: "kube-c-1", noServer: true, want: caProd},
		{name: "no --server: an identified entry at another URL", wantName: "prod-elsew-c-1", noServer: true, wantCode: clierr.CodeConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := tokenCmd{dir: dir, store: config.NewFileStore(dir)}
			flagServer := "https://r.ex"
			if tc.noServer {
				flagServer = ""
			}
			_, _, _, source, _, err := cmd.resolveConnWithCA(flagServer, "c-1", tc.cacert, tc.wantName, tc.shim)
			if tc.wantCode != "" {
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "%v", err)
				assert.Equal(t, tc.wantCode, ce.Code)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, source)
		})
	}
}

// TestStoredCredTokenStopsWhenConfigChangedUnderTheCall asserts the two reads
// of config.yaml in one call agree on the entry: the connection was set up
// (its CA chosen) for one identification, and if the credential lookup
// identifies another, the file changed between the reads and the call stops
// with a retryable error rather than pair one entry's trust with another's
// login.
func TestStoredCredTokenStopsWhenConfigChangedUnderTheCall(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{Servers: map[string]config.Server{
		"prod":    {URL: "https://r.ex"},
		"prod-eu": {URL: "https://r.ex"},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	run := func(resolved entryChoice) error {
		cmd := tokenCmd{dir: dir, store: store, resolved: resolved}
		_, _, _, err := cmd.storedCredToken(t.Context(), failingTransportClient(t), "https://r.ex", "c-1", "prod-eu-c-1", false, ui.Discard())
		return err
	}

	err := run(entryChoice{known: true, name: "prod"})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Contains(t, ce.Message, "'prod'")
	assert.Contains(t, ce.Message, "'prod-eu'")

	err = run(entryChoice{known: true, name: "prod-eu"})
	require.ErrorIs(t, err, errNoStoredCredential, "the same identification goes on to the lookup")
	err = run(entryChoice{})
	require.ErrorIs(t, err, errNoStoredCredential, "no earlier identification, nothing to compare")
}

// TestCandidateRanksFillTheScan asserts the ranker and the scan agree: every
// combination of provenance tier and place among the tier's entries gets a
// distinct rank below candidateRankCount, and none of the ranks is unused, so
// a credentialed entry at any rank is found.
func TestCandidateRanksFillTheScan(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{CurrentServer: "user-current", Servers: map[string]config.Server{
		"user-current":   {URL: "https://r.ex"},
		"user-other":     {URL: "https://r.ex"},
		"plugin-current": {URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin},
		"plugin-other":   {URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin},
	}}
	seen := map[int]string{}
	for _, current := range []string{"user-current", "plugin-current"} {
		cfg.CurrentServer = current
		rankOf := candidateRanker(cfg, false)
		for name := range cfg.Servers {
			if name != current && (name == "user-current" || name == "plugin-current") {
				continue
			}
			r := rankOf(name)
			require.Less(t, r, candidateRankCount, name)
			seen[r] += " " + name
		}
	}
	assert.Len(t, seen, candidateRankCount, "every rank the scan visits is used: %v", seen)
}

// TestNotNamedRemedyQuotesTheClusterInTheNameSuffix asserts the --name remedy
// stays one shell word for a cluster value with a space, as the same
// message's --cluster value does.
func TestNotNamedRemedyQuotesTheClusterInTheNameSuffix(t *testing.T) {
	t.Parallel()
	hits := []storedCredential{{name: "prod", entry: config.Server{URL: "https://r.ex"}}, {name: "prod-eu", entry: config.Server{URL: "https://r.ex"}}}
	msg := entryNotNamedError(&config.Config{}, hits, "my cluster", false).Message
	assert.Contains(t, msg, "--name <entry>-'my cluster'")
	assert.Contains(t, msg, clierr.Rancher+" kubeconfig --cluster 'my cluster'")
	plain := entryNotNamedError(&config.Config{}, hits, "c-1", false).Message
	assert.Contains(t, plain, "--name <entry>-c-1")
}

// failingTransport fails the test if anything is sent through it.
type failingTransport struct{ t *testing.T }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Error("a request was sent")
	return nil, errors.New("no request expected")
}

// TestCallStopsWhenConfigChangesBetweenItsTwoReads asserts, end to end, that
// the call sets its connection up under one identification (the CA is chosen
// from prod-eu), config.yaml gains an entry named exactly like the exec entry's
// --name before the credential lookup reads it again, and the call stops with
// the retryable CONFLICT before any login or request, instead of pairing
// prod-eu's trust with the new entry's login.
func TestCallStopsWhenConfigChangesBetweenItsTwoReads(t *testing.T) {
	t.Setenv(execInfoEnv, "")
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{Servers: map[string]config.Server{
		"prod":    {URL: "https://r.ex"},
		"prod-eu": {URL: "https://r.ex"},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod-eu", URL: "https://r.ex"}, "tok-eu"))

	cmd := tokenCmd{dir: dir, store: store, anchor: anchorFor(dir)}
	cmd.newClient = func(httpx.Options) (*http.Client, error) {
		// The client is built between the two reads of config.yaml.
		changed, err := config.Load(config.FilePath(dir))
		require.NoError(t, err)
		changed.Servers["prod-eu-c-test"] = config.Server{URL: "https://r.ex"}
		require.NoError(t, changed.Write())
		return &http.Client{Transport: failingTransport{t}}, nil
	}

	_, err := cmd.Execute(t.Context(), tin{s: map[string]string{
		"server": "https://r.ex", "cluster": "c-test", "name": "prod-eu-c-test", "auth-provider": "localProvider",
	}}, ui.New(io.Discard, strings.NewReader(""), false, false))
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Contains(t, ce.Message, "config.yaml changed while this call was running")
	assert.Contains(t, ce.Message, "'prod-eu'")
	assert.Contains(t, ce.Message, "'prod-eu-c-test'")
}
