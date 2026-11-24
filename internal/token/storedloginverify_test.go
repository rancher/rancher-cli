package token

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"iter"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// clientRecorder records, for every client a test's command builds through
// its client seam, whether the client skips TLS verification, and for every
// request one of those clients sends, the bearer it carries. When the test
// ends it fails the test if a request whose bearer is a stored login of the
// test's directory went over a client that skips verification, unless the
// login's entry is a plugin login entry that has no saved CA and whose login
// came from a sign-in that did not verify the server.
type clientRecorder struct {
	dir   string
	store config.CredStore
	// report fails the test: t.Errorf for a test's own recorder.
	report func(format string, args ...any)

	mu    sync.Mutex
	sends []recordedSend
}

// recordedSend is one request a recorded client sent.
type recordedSend struct {
	insecure bool
	bearer   string
}

// newClientRecorder starts recording for the directory dir, whose stored
// logins store holds.
func newClientRecorder(t *testing.T, dir string, store config.CredStore) *clientRecorder {
	t.Helper()
	r := &clientRecorder{dir: dir, store: store, report: t.Errorf}
	t.Cleanup(r.check)
	return r
}

// clientsOver is the clients value of a call without --skip-verify whose
// every client is c, for a test that calls storedCredToken or pluginLogin
// itself with a client of its own. It never builds a client that skips
// verification.
func clientsOver(c *http.Client) *callClients {
	return &callClients{clientFor: func(httpx.Options) (*http.Client, error) { return c, nil }}
}

// recordedClients is the one form a test sets tokenCmd.newClient with:
// build makes each client, and rec records it and every request it sends.
// TestNoTestInjectsAClientWithoutTheHelper fails on any other form.
func recordedClients(rec *clientRecorder, build func(httpx.Options) (*http.Client, error)) func(httpx.Options) (*http.Client, error) {
	return func(o httpx.Options) (*http.Client, error) {
		c, err := build(o)
		if err != nil || c == nil {
			return c, err
		}
		next := c.Transport
		if next == nil {
			// What an http.Client with no transport sends through.
			next = http.DefaultTransport
		}
		wrapped := *c
		wrapped.Transport = &recordingTransport{rec: rec, insecure: o.Insecure, next: next}
		return &wrapped, nil
	}
}

type recordingTransport struct {
	rec      *clientRecorder
	insecure bool
	next     http.RoundTripper
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	rt.rec.mu.Lock()
	rt.rec.sends = append(rt.rec.sends, recordedSend{insecure: rt.insecure, bearer: bearer})
	rt.rec.mu.Unlock()
	if rt.insecure && bearer != "" {
		// Checked when it is sent as well as when the test ends: a refresh
		// can replace the stored login, or mark its entry, before then. It
		// reads the recorder's own store. An entry with no stored login is
		// skipped; a store that cannot be read fails the test, naming the
		// store.
		if name, ok := storedLogins(rt.rec.dir, rt.rec.store, rt.rec.report)[bearer]; ok {
			rt.rec.report("the stored login of server entry %q was sent over a client that skips TLS verification", name)
		}
	}
	return rt.next.RoundTrip(r)
}

// insecureSends is the bearer of every request that went over a client
// that skips verification, "" for a request with none.
func (r *clientRecorder) insecureSends() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.sends {
		if s.insecure {
			out = append(out, s.bearer)
		}
	}
	return out
}

// sentVerified reports whether a request carried bearer over a client that
// verifies the server.
func (r *clientRecorder) sentVerified(bearer string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.sends, func(s recordedSend) bool { return !s.insecure && s.bearer == bearer })
}

// check is the rule every recorded test is held to, read against the
// stored logins the directory holds when the test ends.
func (r *clientRecorder) check() {
	entries := storedLogins(r.dir, r.store, r.report)
	for _, bearer := range r.insecureSends() {
		if name, ok := entries[bearer]; ok {
			r.report("the stored login of server entry %q was sent over a client that skips TLS verification", name)
		}
	}
}

// storedLogins maps every stored login of dir that store holds to its
// entry, leaving out the one exception: a plugin login entry that has no
// saved CA and whose login came from a sign-in that did not verify the
// server. An entry with no stored login (config.ErrNoToken) is skipped. A
// config.yaml or a store that cannot be read is reported through report,
// naming the store, and fails the test; it is never skipped.
func storedLogins(dir string, store config.CredStore, report func(format string, args ...any)) map[string]string {
	entries := map[string]string{}
	cfg, err := config.Load(config.FilePath(dir))
	if err != nil {
		report("the recorder cannot read config.yaml of %s to find the stored logins of store %T: %v", dir, store, err)
		return entries
	}
	for name, s := range cfg.Servers {
		if s.Provenance == clierr.ProvenanceExecLogin && s.SignedInWithSkipVerify && s.CA == "" {
			continue
		}
		tok, gerr := store.GetToken(config.ServerRef{Name: name, URL: s.URL})
		switch {
		case errors.Is(gerr, config.ErrNoToken):
		case gerr != nil:
			report("the recorder cannot read the stored login of server entry %q from store %T: %v", name, store, gerr)
		case tok != "":
			entries[tok] = name
		}
	}
	return entries
}

// recorder is the fixture's client recorder, made on first use.
func (x *pluginFixture) recorder() *clientRecorder {
	if x.rec == nil {
		x.rec = newClientRecorder(x.t, x.dir, x.store)
	}
	return x.rec
}

// impostor is a Rancher facade behind TLS with a certificate of its own,
// which no trust store holds, that records the Authorization header of
// every request it gets.
type impostor struct {
	f   *testrancher.Facade
	srv *httptest.Server
	url string
	// caPEM is the PEM of the certificate it presents.
	caPEM string
	// swap, once set, makes every new connection get other, a certificate
	// no client of the test trusts.
	swap  atomic.Bool
	other tls.Certificate
	// swapOn, when set, is the bearer whose first request sets swap.
	swapOn atomic.Pointer[string]

	mu   sync.Mutex
	auth []string
}

func newImpostor(t *testing.T, opts testrancher.Options) *impostor {
	t.Helper()
	cert, pemText, err := testrancher.LoopbackTLSCertificate()
	require.NoError(t, err)
	other, _, err := testrancher.LoopbackTLSCertificate()
	require.NoError(t, err)
	x := &impostor{caPEM: pemText, other: other}
	opts.RequestHook = func(r *http.Request) {
		x.mu.Lock()
		x.auth = append(x.auth, r.Header.Get("Authorization"))
		x.mu.Unlock()
		if w := x.swapOn.Load(); w != nil && r.Header.Get("Authorization") == "Bearer "+*w {
			x.swap.Store(true)
		}
	}
	x.f = newFacadeWithClusters(&opts)
	x.srv = httptest.NewUnstartedServer(x.f)
	// A client that dials an IP address sends no server name, and then
	// only the configuration's own certificates are offered.
	x.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		if x.swap.Load() {
			return &tls.Config{Certificates: []tls.Certificate{x.other}, NextProtos: []string{"http/1.1"}}, nil
		}
		return nil, nil
	}}
	x.srv.StartTLS()
	t.Cleanup(x.srv.Close)
	x.url = x.srv.URL
	return x
}

// login signs in to the impostor over a client that trusts it and returns
// the login credential, which the facade accepts as a bearer. The request
// record starts empty after it.
func (x *impostor) login(t *testing.T) string {
	t.Helper()
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM([]byte(x.caPEM)))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	cred, err := AcquireLoginCredential(t.Context(), client, LoginRequest{CAFlag: "--cacert", Server: x.url, AuthProvider: "localProvider", ResponseType: "json"}, 0,
		ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true))
	require.NoError(t, err)
	client.CloseIdleConnections()
	x.reset()
	return cred.Token
}

func (x *impostor) reset() {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.auth = nil
}

// requests is the number of requests the impostor got.
func (x *impostor) requests() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.auth)
}

// received reports whether a request carried bearer.
func (x *impostor) received(bearer string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return slices.Contains(x.auth, "Bearer "+bearer)
}

// skipVerifyDir writes a config.yaml with token reuse on, the file store
// and entries, and stores each entry's credential from tokens.
func skipVerifyDir(t *testing.T, entries map[string]config.Server, tokens map[string]string) (string, config.CredStore) {
	t.Helper()
	t.Setenv(config.EnvCredentialStore, "")
	t.Setenv(config.EnvCACert, "")
	t.Setenv(execInfoEnv, "")
	dir := t.TempDir()
	cfg := &config.Config{Servers: entries, TokenReuse: true, CredentialStore: "file"}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(dir)
	for name, tok := range tokens {
		require.NoError(t, store.SetToken(config.ServerRef{Name: name, URL: entries[name].URL}, tok))
	}
	return dir, store
}

// legacyToken runs the legacy token command in dir with the flags of the
// row.
func legacyToken(t *testing.T, dir string, store config.CredStore, flags map[string]string, skipVerify bool) (runtime.Result, error) {
	t.Helper()
	in := map[string]string{"user": "alice", "auth-provider": "localProvider"}
	maps.Copy(in, flags)
	cmd := NewLegacyTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)
	return cmd.Execute(t.Context(), tin{s: in, b: map[string]bool{"skip-verify": skipVerify}}, localPrompter())
}

// tlsRefusal is err as the TLS_TRUST error the call must fail with, and
// its message with every remedy command written out.
func tlsRefusal(t *testing.T, err error) (*clierr.Error, string) {
	t.Helper()
	require.Error(t, err)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	require.Equal(t, clierr.CodeTLSTrust, ce.Code, "the call fails with TLS_TRUST: %s", ce.Message)
	return ce, clierr.ExpandRemedies(ce.Message, "")
}

// storedLoginLead is the start of the refusal of entry's stored login.
func storedLoginLead(entry string) string {
	return "the stored login of server entry " + clierr.ShellQuote(entry) +
		" is sent only over a connection that verifies the server, and --skip-verify does not change that; "
}

// execLoginEntryAt is a plugin login entry at url with the CA ca, signed in
// with --skip-verify when marked.
func execLoginEntryAt(url, ca string, marked bool) config.Server {
	return config.Server{URL: url, CA: ca, Provenance: clierr.ProvenanceExecLogin, SignedInWithSkipVerify: marked,
		TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-stored"}, TokenUserID: "user-e2e"}
}

// userEntryAt is a user entry at url with the CA ca.
func userEntryAt(url, ca string) config.Server {
	return config.Server{URL: url, CA: ca, Provenance: clierr.ProvenanceDerived,
		TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-stored"}, TokenUserID: "user-e2e"}
}

// TestTheStoredLoginIsSentOnlyOverAVerifiedConnection asserts, against a
// server whose certificate no trust store holds, that the legacy token
// command sends a stored login only over a connection that verified the
// server, --skip-verify or not. The one exception is a plugin login entry
// that has no saved CA and whose login came from a sign-in that did not
// verify the server, reused by a call that passes --skip-verify. A refused
// stored login fails the call with TLS_TRUST, names the entry and the
// command that saves its CA, and for a plugin login entry the command that
// deletes it.
func TestTheStoredLoginIsSentOnlyOverAVerifiedConnection(t *testing.T) {
	const stored = "token-stored:never-created"
	otherCA := selfSignedCAPEM(t)

	refused := func(t *testing.T, x *impostor, err error, entry string, lead bool) string {
		t.Helper()
		_, msg := tlsRefusal(t, err)
		assert.Zero(t, x.requests(), "no request reached the server")
		if lead {
			assert.Contains(t, msg, storedLoginLead(entry))
		} else {
			assert.NotContains(t, msg, "--skip-verify does not change that")
			assert.Contains(t, msg, "pass it with --cacerts", "a call without --skip-verify names its own remedy")
		}
		return msg
	}

	t.Run("a user entry with a CA that does not match", func(t *testing.T) {
		x := newImpostor(t, testrancher.Options{})
		dir, store := skipVerifyDir(t, map[string]config.Server{"prod": userEntryAt(x.url, otherCA)}, map[string]string{"prod": stored})
		_, err := legacyToken(t, dir, store, map[string]string{"server": x.url, "cluster": "c-test"}, true)
		msg := refused(t, x, err, "prod", true)
		assert.Contains(t, msg, "rancher config update-server 'prod' --cacert <file>")
	})
	t.Run("a user entry, no cluster", func(t *testing.T) {
		x := newImpostor(t, testrancher.Options{})
		dir, store := skipVerifyDir(t, map[string]config.Server{"prod": userEntryAt(x.url, otherCA)}, map[string]string{"prod": stored})
		_, err := legacyToken(t, dir, store, map[string]string{"server": x.url}, true)
		msg := refused(t, x, err, "prod", true)
		assert.Contains(t, msg, "rancher config update-server 'prod' --cacert <file>")
	})
	t.Run("a user entry with no CA", func(t *testing.T) {
		x := newImpostor(t, testrancher.Options{})
		dir, store := skipVerifyDir(t, map[string]config.Server{"prod": userEntryAt(x.url, "")}, map[string]string{"prod": stored})
		_, err := legacyToken(t, dir, store, map[string]string{"server": x.url, "cluster": "c-test"}, true)
		// The system roots verify through the platform verifier, whose
		// unknown-authority text differs by operating system.
		_, _ = tlsRefusal(t, err)
		assert.Zero(t, x.requests(), "no request reached the server")
	})
	t.Run("a user entry on a Rancher without ext tokens", func(t *testing.T) {
		x := newImpostor(t, testrancher.Options{Legacy: true})
		dir, store := skipVerifyDir(t, map[string]config.Server{"prod": userEntryAt(x.url, otherCA)}, map[string]string{"prod": stored})
		_, err := legacyToken(t, dir, store, map[string]string{"server": x.url, "cluster": "c-test"}, true)
		refused(t, x, err, "prod", true)
	})
	t.Run("a plugin login entry not signed in with --skip-verify", func(t *testing.T) {
		x := newImpostor(t, testrancher.Options{})
		name := config.ExecLoginEntryName(x.url)
		dir, store := skipVerifyDir(t, map[string]config.Server{name: execLoginEntryAt(x.url, "", false)}, map[string]string{name: stored})
		_, err := legacyToken(t, dir, store, map[string]string{"server": x.url, "cluster": "c-test"}, true)
		msg := refused(t, x, err, name, true)
		assert.Contains(t, msg, "rancher config update-server "+clierr.ShellQuote(name)+" --cacert <file>")
		assert.Contains(t, msg, "; or delete server entry "+clierr.ShellQuote(name)+" with: rancher config delete-server "+clierr.ShellQuote(name)+"; then run kubectl again to sign in with --skip-verify;")
	})
	t.Run("a plugin login entry signed in with --skip-verify", func(t *testing.T) {
		x := newImpostor(t, testrancher.Options{})
		login := x.login(t)
		name := config.ExecLoginEntryName(x.url)
		dir, store := skipVerifyDir(t, map[string]config.Server{name: execLoginEntryAt(x.url, "", true)}, map[string]string{name: login})
		_, err := legacyToken(t, dir, store, map[string]string{"server": x.url, "cluster": "c-test"}, true)
		require.NoError(t, err)
		assert.True(t, x.received(login), "the entry's own login is reused without verifying the server")
	})
	t.Run("a plugin login entry signed in with --skip-verify that has a CA", func(t *testing.T) {
		x := newImpostor(t, testrancher.Options{})
		name := config.ExecLoginEntryName(x.url)
		dir, store := skipVerifyDir(t, map[string]config.Server{name: execLoginEntryAt(x.url, otherCA, true)}, map[string]string{name: stored})
		_, err := legacyToken(t, dir, store, map[string]string{"server": x.url, "cluster": "c-test"}, true)
		refused(t, x, err, name, true)
	})
	t.Run("a user entry that carries the record", func(t *testing.T) {
		x := newImpostor(t, testrancher.Options{})
		e := userEntryAt(x.url, "")
		e.SignedInWithSkipVerify = true
		dir, store := skipVerifyDir(t, map[string]config.Server{"prod": e}, map[string]string{"prod": stored})
		_, err := legacyToken(t, dir, store, map[string]string{"server": x.url, "cluster": "c-test"}, true)
		_, _ = tlsRefusal(t, err)
		assert.Zero(t, x.requests(), "no request reached the server")
	})
	t.Run("a plugin login entry signed in with --skip-verify, a call without it", func(t *testing.T) {
		x := newImpostor(t, testrancher.Options{})
		name := config.ExecLoginEntryName(x.url)
		dir, store := skipVerifyDir(t, map[string]config.Server{name: execLoginEntryAt(x.url, "", true)}, map[string]string{name: stored})
		_, err := legacyToken(t, dir, store, map[string]string{"server": x.url, "cluster": "c-test"}, false)
		msg := refused(t, x, err, name, false)
		assert.Contains(t, msg, "pass it with --cacerts", "a call without --skip-verify names its own remedy")
		assert.NotContains(t, msg, "save the CA on server entry", "a call without --skip-verify names its own remedy only")
		assert.False(t, x.received(stored))
	})
	t.Run("a user entry whose CA matches", func(t *testing.T) {
		x := newImpostor(t, testrancher.Options{})
		login := x.login(t)
		dir, store := skipVerifyDir(t, map[string]config.Server{"prod": userEntryAt(x.url, x.caPEM)}, map[string]string{"prod": login})
		_, err := legacyToken(t, dir, store, map[string]string{"server": x.url, "cluster": "c-test"}, true)
		require.NoError(t, err)
		assert.True(t, x.received(login), "the stored login is sent over the connection the saved CA verified")
	})
	t.Run("a user entry whose CA matches, the CA read from a plugin login entry that sorts first", func(t *testing.T) {
		// The call reads the CA of the first entry by name with the URL, the
		// plugin login entry, which has none; the server accepts the user entry's
		// login. The remedy saves the CA where the call reads it.
		x := newImpostor(t, testrancher.Options{})
		name := config.ExecLoginEntryName(x.url)
		require.Less(t, name, "prod")
		dir, store := skipVerifyDir(t, map[string]config.Server{"prod": userEntryAt(x.url, x.caPEM), name: execLoginEntryAt(x.url, "", true)},
			map[string]string{"prod": stored})
		_, err := legacyToken(t, dir, store, map[string]string{"server": x.url, "cluster": "c-test"}, true)
		msg := refused(t, x, err, "prod", true)
		assert.Contains(t, msg, ", or save the CA on server entry "+clierr.ShellQuote(name)+" with: rancher config update-server "+clierr.ShellQuote(name)+" --cacert <file>;")
		assert.NotContains(t, msg, "update-server 'prod'")
	})
	t.Run("the plugin's own sign-in with --skip-verify", func(t *testing.T) {
		for _, cacerts := range []string{"", otherCA} {
			t.Run(map[bool]string{true: "no --cacerts", false: "with --cacerts"}[cacerts == ""], func(t *testing.T) {
				x := newImpostor(t, testrancher.Options{})
				dir, store := skipVerifyDir(t, map[string]config.Server{}, nil)
				flags := map[string]string{"server": x.url, "cluster": "c-test"}
				if cacerts != "" {
					flags["cacerts"] = cacerts
				}
				_, err := legacyToken(t, dir, store, flags, true)
				require.NoError(t, err)
				require.Equal(t, 1, x.f.LoginCount())
				cfg, err := config.Load(config.FilePath(dir))
				require.NoError(t, err)
				name := config.ExecLoginEntryName(x.url)
				e, ok := cfg.Servers[name]
				require.True(t, ok, "the plugin wrote its login entry")
				assert.True(t, e.SignedInWithSkipVerify, "the entry records the sign-in skipped verification")
				assert.Empty(t, e.CA, "a CA the sign-in did not verify with is not saved")

				login, err := store.GetToken(config.ServerRef{Name: name, URL: e.URL})
				require.NoError(t, err)
				x.reset()
				flags["user"] = "bob"
				_, err = legacyToken(t, dir, store, flags, true)
				require.NoError(t, err)
				assert.Equal(t, 1, x.f.LoginCount(), "the second call reuses the login")
				assert.True(t, x.received(login))
			})
		}
	})
	t.Run("the login found by the read under the plugin login lock", func(t *testing.T) {
		x := newImpostor(t, testrancher.Options{})
		name := config.ExecLoginEntryName(x.url)
		dir, store := skipVerifyDir(t, map[string]config.Server{name: execLoginEntryAt(x.url, "", false)}, map[string]string{name: stored})
		cmd := tokenCmd{dir: dir, store: config.NewFileStore(t.TempDir()), anchor: anchorFor(dir),
			openStore: func(string, string) (config.CredStore, string, error) { return store, "file", nil }}
		_, err := cmd.runWithParams(t.Context(), runParams{Server: x.url, Name: config.LegacyCachePrefix + "alice", Cluster: "c-test",
			AuthProvider: "localProvider", SkipVerify: true, legacyShim: true}, localPrompter())
		_, _ = tlsRefusal(t, err)
		assert.Zero(t, x.requests(), "no request reached the server")
	})
	t.Run("a plugin login entry signed in with --skip-verify whose login the server rejects", func(t *testing.T) {
		x := newImpostor(t, testrancher.Options{})
		name := config.ExecLoginEntryName(x.url)
		dir, store := skipVerifyDir(t, map[string]config.Server{name: execLoginEntryAt(x.url, "", true)}, map[string]string{name: stored})
		_, err := legacyToken(t, dir, store, map[string]string{"server": x.url, "cluster": "c-test"}, true)
		require.NoError(t, err)
		assert.Equal(t, 1, x.f.LoginCount(), "the plugin signs in once to refresh the entry")
		cfg, err := config.Load(config.FilePath(dir))
		require.NoError(t, err)
		e := cfg.Servers[name]
		login, err := store.GetToken(config.ServerRef{Name: name, URL: e.URL})
		require.NoError(t, err)
		assert.NotEqual(t, stored, login, "the entry holds the new login")
		assert.True(t, e.SignedInWithSkipVerify, "the refreshed entry keeps the record")
		assert.Empty(t, e.CA)
	})
	t.Run("the sign-in after the server rejected a login sent over a verified connection", func(t *testing.T) {
		x := newImpostor(t, testrancher.Options{})
		x.srv.Config.SetKeepAlivesEnabled(false)
		dir, store := skipVerifyDir(t, map[string]config.Server{"prod": userEntryAt(x.url, x.caPEM)}, map[string]string{"prod": stored})
		// After the server rejects the stored login every new connection
		// gets a certificate nothing trusts: a sign-in over a verified
		// connection fails, one that skips verification would succeed.
		x.swapAfter(stored)
		_, err := legacyToken(t, dir, store, map[string]string{"server": x.url, "cluster": "c-test"}, true)
		_, _ = tlsRefusal(t, err)
		assert.True(t, x.received(stored), "the stored login went over the verified connection")
		assert.Zero(t, x.f.LoginCount(), "the sign-in did not skip verification")
	})
}

// swapAfter makes the impostor present the untrusted certificate on every
// connection made after a request carried bearer.
func (x *impostor) swapAfter(bearer string) {
	x.swapOn.Store(&bearer)
}

// TestNoStoredLoginGoesOverTheUnverifiedClient asserts, for the lanes the
// commands reach only through combinations or states a real server cannot
// easily make, that every request with a stored login goes over
// a client that verifies the server when the call passes --skip-verify.
func TestNoStoredLoginGoesOverTheUnverifiedClient(t *testing.T) {
	// seeded is an in-memory fixture with a user entry whose stored login
	// the facade accepts.
	seeded := func(t *testing.T, opts testrancher.Options, entry config.Server) (*pluginFixture, string) {
		t.Helper()
		x := newInMemoryPluginFixture(t, true, opts)
		cred, err := AcquireLoginCredential(t.Context(), x.client(), LoginRequest{CAFlag: "--cacert", Server: x.url, AuthProvider: "localProvider", ResponseType: "json"}, 0, localPrompter())
		require.NoError(t, err)
		entry.URL = x.url
		seedEntry(t, x.dir, x.store, "prod", entry, cred.Token, true)
		return x, cred.Token
	}
	noInsecureStoredLogin := func(t *testing.T, x *pluginFixture, login string) {
		t.Helper()
		rec := x.recorder()
		assert.NotContains(t, rec.insecureSends(), login, "the stored login went over a client that skips verification")
		assert.True(t, rec.sentVerified(login), "the stored login went over a client that verifies the server")
	}

	for _, authProvider := range []string{"", "local"} {
		t.Run(fmt.Sprintf("--user-id, auth provider %q", authProvider), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				t.Setenv(execInfoEnv, "")
				x, login := seeded(t, testrancher.Options{}, userEntryAt("", ""))
				_, err := x.command(x.store).runWithParams(t.Context(), runParams{Server: x.url, Cluster: "c-test", UserID: "user-e2e",
					AuthProvider: authProvider, SkipVerify: true}, localPrompter())
				require.NoError(t, err)
				noInsecureStoredLogin(t, x, login)
			})
		})
	}
	t.Run("the install id request after the server accepted a stored login", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			t.Setenv(execInfoEnv, "")
			rec := &installIDServer{}
			x, login := seeded(t, testrancher.Options{Fallthrough: rec}, userEntryAt("", ""))
			rec.f = x.f
			clearRecords(t)
			_, err := x.command(x.store).runWithParams(t.Context(), runParams{Server: x.url, Name: config.LegacyCachePrefix + "alice", Cluster: "c-test",
				AuthProvider: "localProvider", SkipVerify: true, legacyShim: true}, localPrompter())
			require.NoError(t, err)
			fetchRegistered(t, registeredFetcher(t))
			require.Contains(t, rec.bearers(), login, "the install id request carried the stored login")
			noInsecureStoredLogin(t, x, login)
		})
	})
	t.Run("the stored login sent to a Rancher without ext tokens", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			t.Setenv(execInfoEnv, "")
			x, login := seeded(t, testrancher.Options{Legacy: true}, userEntryAt("", ""))
			res, err := x.command(x.store).runWithParams(t.Context(), runParams{Server: x.url, Name: config.LegacyCachePrefix + "alice", Cluster: "c-test",
				AuthProvider: "localProvider", SkipVerify: true, legacyShim: true}, localPrompter())
			require.NoError(t, err)
			assert.Equal(t, login, res.Data.(*execCredential).Status.Token)
			noInsecureStoredLogin(t, x, login)
		})
	})
	rejected := func(t *testing.T, reread bool) {
		t.Helper()
		synctest.Test(t, func(t *testing.T) {
			t.Setenv(execInfoEnv, "")
			x := newInMemoryPluginFixture(t, true, testrancher.Options{})
			name := config.ExecLoginEntryName(x.url)
			const stored = "token-stored:never-created"
			seedEntry(t, x.dir, x.store, name, execLoginEntryAt(x.url, "", false), stored, true)
			cmd := x.command(x.store)
			if reread {
				// The command's own store misses the login; the read under
				// the plugin login lock opens the store that holds it.
				cmd.store = config.NewFileStore(t.TempDir())
				cmd.openStore = func(string, string) (config.CredStore, string, error) { return x.store, "file", nil }
			}
			_, err := cmd.runWithParams(t.Context(), runParams{Server: x.url, Name: config.LegacyCachePrefix + "alice", Cluster: "c-test",
				AuthProvider: "localProvider", SkipVerify: true, legacyShim: true}, localPrompter())
			require.NoError(t, err)
			assert.Equal(t, 1, x.f.LoginCount(), "the plugin signed in once")
			rec := x.recorder()
			assert.True(t, rec.sentVerified(stored), "the rejected login went over a client that verifies the server")
			assert.Empty(t, rec.insecureSends(), "the sign-in after the rejection went over a client that verifies the server")
			e, ok := x.entry(t)
			require.True(t, ok)
			assert.False(t, e.SignedInWithSkipVerify, "a sign-in over a verified connection is not recorded as one that skipped verification")
		})
	}
	t.Run("the plugin refresh after the server rejected the login", func(t *testing.T) { rejected(t, false) })
	t.Run("the sign-in after token reuse was turned off while the refresh waited", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			t.Setenv(execInfoEnv, "")
			const stored = "token-stored:never-created"
			var dir atomic.Pointer[string]
			// The server's 401 to the stored login comes with the setting
			// turned off: the refresh finds it off and the call signs in.
			hook := func(r *http.Request) {
				d := dir.Load()
				if d == nil || r.Header.Get("Authorization") != "Bearer "+stored {
					return
				}
				cfg, err := config.Load(config.FilePath(*d))
				if err == nil && cfg.TokenReuse {
					cfg.TokenReuse = false
					cfg.SetPath(config.FilePath(*d))
					_ = cfg.Write()
				}
			}
			x := newInMemoryPluginFixture(t, true, testrancher.Options{RequestHook: hook})
			name := config.ExecLoginEntryName(x.url)
			seedEntry(t, x.dir, x.store, name, execLoginEntryAt(x.url, "", false), stored, true)
			dir.Store(&x.dir)
			_, err := x.command(x.store).runWithParams(t.Context(), runParams{Server: x.url, Name: config.LegacyCachePrefix + "alice", Cluster: "c-test",
				AuthProvider: "localProvider", SkipVerify: true, legacyShim: true}, localPrompter())
			require.NoError(t, err)
			assert.Equal(t, 1, x.f.LoginCount(), "the call signed in once")
			rec := x.recorder()
			assert.True(t, rec.sentVerified(stored), "the rejected login went over a client that verifies the server")
			assert.Empty(t, rec.insecureSends(), "the sign-in after the rejection went over a client that verifies the server")
		})
	})
	t.Run("the rejection of the login the read under the plugin login lock found", func(t *testing.T) { rejected(t, true) })
}

// memoryLoginStore is a store that is not the directory's file store: it
// holds stored logins in memory, and fails every read with err when it is set.
type memoryLoginStore struct {
	config.CredStore
	logins map[string]string
	err    error
}

func (s memoryLoginStore) GetToken(ref config.ServerRef) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	tok, ok := s.logins[ref.Name]
	if !ok {
		return "", config.ErrNoToken
	}
	return tok, nil
}

// TestTheRecorderChecksEveryClientAndEveryRequest asserts the recorder
// records a client its builder made with no transport, and fails a request
// that carries a stored login over a client that skips verification when it
// is sent, even when the test replaces that login before it ends. It reads
// the stored logins from its own store, skips an entry that has none, and
// fails the test, naming the store, for a store it cannot read.
func TestTheRecorderChecksEveryClientAndEveryRequest(t *testing.T) {
	t.Setenv(config.EnvCredentialStore, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(srv.Close)
	// send sends one request with bearer through c.
	send := func(t *testing.T, c *http.Client, bearer string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := c.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
	}
	dir, store := skipVerifyDir(t, map[string]config.Server{"prod": userEntryAt("https://r.example.com", "")}, map[string]string{"prod": "stored-login"})
	var reported []string
	rec := &clientRecorder{dir: dir, store: store, report: func(format string, args ...any) { reported = append(reported, fmt.Sprintf(format, args...)) }}

	t.Run("a client its builder made with no transport", func(t *testing.T) {
		build := recordedClients(rec, func(httpx.Options) (*http.Client, error) { return &http.Client{}, nil })
		c, err := build(httpx.Options{Insecure: true})
		require.NoError(t, err)
		// The package's tests refuse http.DefaultTransport, which a client
		// with no transport sends through, so no request is sent here.
		rt, ok := c.Transport.(*recordingTransport)
		require.True(t, ok, "the client's requests are recorded")
		assert.Equal(t, http.DefaultTransport, rt.next, "through the transport a client with none uses")
		assert.True(t, rt.insecure)
	})
	t.Run("a stored login replaced before the test ends", func(t *testing.T) {
		reported = nil
		build := recordedClients(rec, func(httpx.Options) (*http.Client, error) { return srv.Client(), nil })
		c, err := build(httpx.Options{Insecure: true})
		require.NoError(t, err)
		send(t, c, "stored-login")
		require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: "https://r.example.com"}, "replaced-login"))
		rec.check()
		assert.Equal(t, []string{`the stored login of server entry "prod" was sent over a client that skips TLS verification`}, reported,
			"the request is checked when it is sent")
	})
	// sendInsecure sends one request with bearer over a client of a recorder
	// over store that skips verification, and returns the recorder and what
	// it has reported.
	sendInsecure := func(t *testing.T, store config.CredStore, bearer string) (*clientRecorder, *[]string) {
		t.Helper()
		got := new([]string)
		own := &clientRecorder{dir: dir, store: store, report: func(format string, args ...any) { *got = append(*got, fmt.Sprintf(format, args...)) }}
		build := recordedClients(own, func(httpx.Options) (*http.Client, error) { return srv.Client(), nil })
		c, err := build(httpx.Options{Insecure: true})
		require.NoError(t, err)
		send(t, c, bearer)
		return own, got
	}
	t.Run("a login held by a store that is not the file store", func(t *testing.T) {
		_, got := sendInsecure(t, memoryLoginStore{logins: map[string]string{"prod": "memory-login"}}, "memory-login")
		assert.Equal(t, []string{`the stored login of server entry "prod" was sent over a client that skips TLS verification`}, *got,
			"the request is checked against the recorder's own store")
	})
	t.Run("an entry with no stored login", func(t *testing.T) {
		own, got := sendInsecure(t, memoryLoginStore{}, "other-login")
		assert.Empty(t, *got, "an entry with no stored login is not an error")
		own.check()
		assert.Empty(t, *got)
	})
	t.Run("a store that cannot be read", func(t *testing.T) {
		own, got := sendInsecure(t, memoryLoginStore{err: errors.New("store offline")}, "any-login")
		require.Len(t, *got, 1, "a store the recorder cannot read fails the test")
		assert.Contains(t, (*got)[0], "cannot read the stored login of server entry \"prod\" from store token.memoryLoginStore: store offline")
		var atEnd []string
		own.report = func(format string, args ...any) { atEnd = append(atEnd, fmt.Sprintf(format, args...)) }
		own.check()
		require.Len(t, atEnd, 1, "the check when the test ends reads the same store")
		assert.Contains(t, atEnd[0], "from store token.memoryLoginStore: store offline")
	})
}

// clientSeamAllowed are the tests that set tokenCmd.newClient without
// recordedClients, by file and enclosing function, with the reason.
var clientSeamAllowed = map[string]string{
	"tlstrust_test.go TestClientForWrapsAnInjectedClientWithoutChangingIt": "a unit test of clientFor itself, which asserts the client it is given comes back unchanged",
}

// clientSeamViolations lists, as "file:line", every place in files (name
// to Go source) that sets tokenCmd.newClient, or builds a callClients with
// clientFor, by anything but a call of recordedClients, outside the
// allowed functions.
func clientSeamViolations(files map[string]string) ([]string, error) {
	var out []string
	for name, src := range files {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			return nil, err
		}
		for decl, body := range declarations(f) {
			if _, allowed := clientSeamAllowed[name+" "+decl]; allowed {
				continue
			}
			ast.Inspect(body, func(n ast.Node) bool {
				bad := func(pos token.Pos) { out = append(out, fmt.Sprintf("%s:%d", name, fset.Position(pos).Line)) }
				switch v := n.(type) {
				case *ast.AssignStmt:
					for i, lhs := range v.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if !ok || sel.Sel.Name != "newClient" {
							continue
						}
						if len(v.Rhs) != len(v.Lhs) || !isRecordedClientsCall(v.Rhs[i]) {
							bad(sel.Pos())
						}
					}
				case *ast.KeyValueExpr:
					if k, ok := v.Key.(*ast.Ident); ok && k.Name == "newClient" && !isRecordedClientsCall(v.Value) {
						bad(v.Pos())
					}
				case *ast.CompositeLit:
					if t, ok := v.Type.(*ast.Ident); ok && t.Name == "callClients" && decl != "clientsOver" {
						for _, e := range v.Elts {
							if kv, ok := e.(*ast.KeyValueExpr); ok {
								if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "clientFor" {
									bad(kv.Pos())
								}
							}
						}
					}
				}
				return true
			})
		}
	}
	slices.Sort(out)
	return out, nil
}

// declarations yields every top-level declaration of f that can hold code,
// by name: a function's or method's body, and each value of a package-level
// var or const, named by its first name.
func declarations(f *ast.File) iter.Seq2[string, ast.Node] {
	return func(yield func(string, ast.Node) bool) {
		for _, d := range f.Decls {
			switch v := d.(type) {
			case *ast.FuncDecl:
				if v.Body != nil && !yield(v.Name.Name, v.Body) {
					return
				}
			case *ast.GenDecl:
				for _, spec := range v.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Names) == 0 {
						continue
					}
					for _, val := range vs.Values {
						if !yield(vs.Names[0].Name, val) {
							return
						}
					}
				}
			}
		}
	}
}

func isRecordedClientsCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	return ok && id.Name == "recordedClients"
}

// TestNoTestInjectsAClientWithoutTheHelper asserts every test of the
// package that injects a client through tokenCmd.newClient does it through
// recordedClients, so the rule that no stored login goes over a client that
// skips verification is checked in every one of them.
func TestNoTestInjectsAClientWithoutTheHelper(t *testing.T) {
	t.Run("the package", func(t *testing.T) {
		entries, err := os.ReadDir(".")
		require.NoError(t, err)
		files := map[string]string{}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			raw, err := os.ReadFile(e.Name())
			require.NoError(t, err)
			files[e.Name()] = string(raw)
		}
		got, err := clientSeamViolations(files)
		require.NoError(t, err)
		assert.Empty(t, got, "set newClient through recordedClients")
	})
	const head = "package token\n\nfunc f(t *testing.T) {\n\tcmd := tokenCmd{}\n"
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"a direct assignment", "\tcmd.newClient = func(httpx.Options) (*http.Client, error) { return nil, nil }\n", 1},
		{"a direct struct field", "\t_ = tokenCmd{newClient: func(httpx.Options) (*http.Client, error) { return nil, nil }}\n", 1},
		{"a clients value with its own builder", "\t_ = &callClients{clientFor: build}\n", 1},
		{"the helper", "\tcmd.newClient = recordedClients(rec, build)\n", 0},
		{"the helper in a struct field", "\t_ = tokenCmd{newClient: recordedClients(rec, build)}\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := clientSeamViolations(map[string]string{"x_test.go": head + tc.body + "\t_ = cmd\n}\n"})
			require.NoError(t, err)
			assert.Len(t, got, tc.want, "%v", got)
		})
	}
	for _, tc := range []struct {
		name, src string
		want      int
	}{
		{"a package-level struct field", "package token\n\nvar cmd = tokenCmd{newClient: func(httpx.Options) (*http.Client, error) { return nil, nil }}\n", 1},
		{"a package-level func literal", "package token\n\nvar inject = func(cmd *tokenCmd) { cmd.newClient = nil }\n", 1},
		{"a package-level helper form", "package token\n\nvar cmd = tokenCmd{newClient: recordedClients(rec, build)}\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := clientSeamViolations(map[string]string{"x_test.go": tc.src})
			require.NoError(t, err)
			assert.Len(t, got, tc.want, "%v", got)
		})
	}
}
