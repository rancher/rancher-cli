package cli

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	tokencomplete "github.com/rancher/rancher-cli/internal/token/complete"
)

// authListServer is a Rancher that answers the unauthenticated provider
// list: the local auth config, an OpenLDAP one, and a GitHub one this CLI
// cannot sign in with. It records the path and the Authorization header of
// every request.
type authListServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
	auths []string
	// status, when set, is the status every request is answered with.
	status int
}

func newAuthListServer(t *testing.T, tls bool) *authListServer {
	t.Helper()
	s := &authListServer{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		status := s.status
		s.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if r.URL.Path != "/v1-public/authproviders" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{
			{"id": "local", "type": "localProvider"},
			{"id": "corp-ldap", "type": "openLdapProvider"},
			{"id": "gh", "type": "githubProvider"},
		}})
	})
	if tls {
		s.srv = httptest.NewTLSServer(h)
	} else {
		s.srv = httptest.NewServer(h)
	}
	t.Cleanup(s.srv.Close)
	return s
}

func (s *authListServer) served() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.paths)
}

func (s *authListServer) authorizations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.auths)
}

// caPEM is the certificate a TLS server presents, as the PEM a CA value
// carries.
func (s *authListServer) caPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.srv.Certificate().Raw}))
}

// caFile writes the server's certificate to a file in dir and returns its
// path.
func (s *authListServer) caFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(p, []byte(s.caPEM()), 0o600))
	return p
}

// completeAuthProvider runs one completion of --auth-provider through the
// real root over a directory holding cfg, with live completion on and no
// credential anywhere, and returns the candidates and the trace. env sets
// environment variables beyond that.
func completeAuthProvider(t *testing.T, cfg *config.Config, env map[string]string, words ...string) (candidates []string, trace string) {
	t.Helper()
	stdout, trace := completeAuthProviderOutput(t, cfg, env, words...)
	return strings.Fields(stdout), trace
}

// completeAuthProviderOutput is completeAuthProvider returning the exact
// bytes written to stdout instead of its fields.
func completeAuthProviderOutput(t *testing.T, cfg *config.Config, env map[string]string, words ...string) (stdout, trace string) {
	t.Helper()
	keyring.MockInit()
	dir := t.TempDir()
	cfg.Completion = config.CompletionConfig{Network: true}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfigForTest(t, cfg, dir)
	t.Setenv(config.EnvConfigDir, dir)
	for _, name := range []string{config.EnvToken, config.EnvURL, config.EnvCACert} {
		t.Setenv(name, "")
	}
	for name, v := range env {
		t.Setenv(name, v)
	}
	t.Setenv(config.EnvCredentialStore, "file")
	t.Setenv(config.EnvCompletionDebug, "1")
	return runCompletionTraced(t, append(words, "--auth-provider"))
}

var authConfigNames = []string{"corp-ldap", "local"}

// TestAuthProviderCandidatesCarryNoDescription asserts the candidates of
// the no-credential path are the bare names, one per line, byte for byte:
// no colon, no description, the same lines before and after descriptions
// existed for other sources.
// NOT parallel: the environment and the stored config are process-wide.
func TestAuthProviderCandidatesCarryNoDescription(t *testing.T) {
	srv := newAuthListServer(t, false)
	cfg := &config.Config{CurrentServer: "cur", Servers: map[string]config.Server{"cur": {URL: srv.srv.URL}}}
	stdout, _ := completeAuthProviderOutput(t, cfg, nil, "auth", "get-token")
	assert.Equal(t, "corp-ldap\nlocal\n", stdout)

	stdout, _ = completeAuthProviderOutput(t, cfg, nil, "auth", "login")
	assert.Equal(t, "corp-ldap\nlocal\n", stdout)

	stdout, _ = completeAuthProviderOutput(t, &config.Config{}, nil, "token", "--server", srv.srv.URL, "--user", "alice")
	assert.Equal(t, "corp-ldap\nlocal\n", stdout)
}

// TestGetTokenCompletesAuthProviderFromItsOwnTarget asserts, through the
// real root, that auth get-token completes --auth-provider from the server
// the command itself would connect to: --server, else RANCHER_CLI_URL, else
// the current server entry, and that --name only chooses the CA.
// NOT parallel: the environment and the stored config are process-wide.
func TestGetTokenCompletesAuthProviderFromItsOwnTarget(t *testing.T) {
	t.Run("--server", func(t *testing.T) {
		current, named := newAuthListServer(t, false), newAuthListServer(t, false)
		got, _ := completeAuthProvider(t, &config.Config{CurrentServer: "cur", Servers: map[string]config.Server{"cur": {URL: current.srv.URL}}}, nil,
			"auth", "get-token", "--server", named.srv.URL)
		assert.Equal(t, authConfigNames, got)
		assert.Equal(t, []string{"/v1-public/authproviders"}, named.served())
		assert.Empty(t, current.served())
	})
	t.Run("RANCHER_CLI_URL over the current server entry", func(t *testing.T) {
		current, fromEnv := newAuthListServer(t, false), newAuthListServer(t, false)
		cfg := &config.Config{CurrentServer: "cur", Servers: map[string]config.Server{"cur": {URL: current.srv.URL}}}
		got, _ := completeAuthProvider(t, cfg, map[string]string{config.EnvURL: fromEnv.srv.URL}, "auth", "get-token")
		assert.Equal(t, authConfigNames, got)
		assert.Equal(t, []string{"/v1-public/authproviders"}, fromEnv.served())
		assert.Empty(t, current.served())
	})
	t.Run("the current server entry", func(t *testing.T) {
		current := newAuthListServer(t, false)
		got, _ := completeAuthProvider(t, &config.Config{CurrentServer: "cur", Servers: map[string]config.Server{"cur": {URL: current.srv.URL}}}, nil,
			"auth", "get-token")
		assert.Equal(t, authConfigNames, got)
		assert.Equal(t, []string{"/v1-public/authproviders"}, current.served())
	})
	t.Run("--name takes the CA of its entry and not its server", func(t *testing.T) {
		tlsServer := newAuthListServer(t, true)
		cfg := &config.Config{CurrentServer: "n", Servers: map[string]config.Server{"n": {URL: tlsServer.srv.URL, CA: tlsServer.caPEM()}}}
		got, _ := completeAuthProvider(t, cfg, nil, "auth", "get-token", "--server", tlsServer.srv.URL, "--name", "n", "--cluster", "c-1")
		assert.Equal(t, authConfigNames, got, "the entry's CA is trusted")

		bare := &config.Config{CurrentServer: "n", Servers: map[string]config.Server{"n": {URL: tlsServer.srv.URL}}}
		got, _ = completeAuthProvider(t, bare, nil, "auth", "get-token", "--server", tlsServer.srv.URL, "--name", "n", "--cluster", "c-1")
		assert.Empty(t, got, "no CA: the server's certificate is not trusted")
	})
	t.Run("--name of an entry at another URL gives nothing", func(t *testing.T) {
		named, other := newAuthListServer(t, false), newAuthListServer(t, false)
		cfg := &config.Config{CurrentServer: "n", Servers: map[string]config.Server{"n": {URL: other.srv.URL}}}
		got, trace := completeAuthProvider(t, cfg, nil, "auth", "get-token", "--server", named.srv.URL, "--name", "n", "--cluster", "c-1")
		assert.Empty(t, got)
		assert.Empty(t, named.served())
		assert.Empty(t, other.served(), "the entry's URL is not used instead")
		assert.Contains(t, trace, "the command names no server to connect to")
	})
	t.Run("--cacert and RANCHER_CLI_CACERT", func(t *testing.T) {
		tlsServer := newAuthListServer(t, true)
		cfg := func() *config.Config { return &config.Config{} }
		got, _ := completeAuthProvider(t, cfg(), nil, "auth", "get-token", "--server", tlsServer.srv.URL, "--cacert", tlsServer.caFile(t))
		assert.Equal(t, authConfigNames, got, "--cacert")
		got, _ = completeAuthProvider(t, cfg(), map[string]string{config.EnvCACert: tlsServer.caFile(t)}, "auth", "get-token", "--server", tlsServer.srv.URL)
		assert.Equal(t, authConfigNames, got, "RANCHER_CLI_CACERT for that server")
		got, _ = completeAuthProvider(t, cfg(), nil, "auth", "get-token", "--server", tlsServer.srv.URL)
		assert.Empty(t, got, "neither: the certificate is not trusted")
	})
}

// TestAuthLoginCompletesAuthProviderFromItsOwnTarget asserts auth login
// completes --auth-provider from the server it would log in to: --url, else
// the entry named, else the current entry, trusting the CA that choice
// uses, and nothing where the command refuses.
// NOT parallel: the environment and the stored config are process-wide.
func TestAuthLoginCompletesAuthProviderFromItsOwnTarget(t *testing.T) {
	t.Run("--url", func(t *testing.T) {
		current, url := newAuthListServer(t, false), newAuthListServer(t, false)
		got, _ := completeAuthProvider(t, &config.Config{CurrentServer: "cur", Servers: map[string]config.Server{"cur": {URL: current.srv.URL}}}, nil,
			"auth", "login", "--url", url.srv.URL)
		assert.Equal(t, authConfigNames, got)
		assert.Equal(t, []string{"/v1-public/authproviders"}, url.served())
		assert.Empty(t, current.served())
	})
	t.Run("the entry named", func(t *testing.T) {
		current, named := newAuthListServer(t, false), newAuthListServer(t, false)
		cfg := &config.Config{CurrentServer: "cur", Servers: map[string]config.Server{"cur": {URL: current.srv.URL}, "prod": {URL: named.srv.URL}}}
		got, _ := completeAuthProvider(t, cfg, nil, "auth", "login", "prod")
		assert.Equal(t, authConfigNames, got)
		assert.Equal(t, []string{"/v1-public/authproviders"}, named.served())
		assert.Empty(t, current.served())
	})
	t.Run("the current entry", func(t *testing.T) {
		current := newAuthListServer(t, false)
		got, _ := completeAuthProvider(t, &config.Config{CurrentServer: "cur", Servers: map[string]config.Server{"cur": {URL: current.srv.URL}}}, nil, "auth", "login")
		assert.Equal(t, authConfigNames, got)
		assert.Equal(t, []string{"/v1-public/authproviders"}, current.served())
	})
	t.Run("no current entry and one entry gives nothing", func(t *testing.T) {
		only := newAuthListServer(t, false)
		got, _ := completeAuthProvider(t, &config.Config{Servers: map[string]config.Server{"only": {URL: only.srv.URL}}}, nil, "auth", "login")
		assert.Empty(t, got, "the command refuses it: login never selects an entry implicitly")
		assert.Empty(t, only.served())
	})
	t.Run("an unknown entry gives nothing", func(t *testing.T) {
		current := newAuthListServer(t, false)
		got, _ := completeAuthProvider(t, &config.Config{CurrentServer: "cur", Servers: map[string]config.Server{"cur": {URL: current.srv.URL}}}, nil, "auth", "login", "nope")
		assert.Empty(t, got)
		assert.Empty(t, current.served())
	})
	t.Run("the CA of the entry named, and of --cacert", func(t *testing.T) {
		tlsServer := newAuthListServer(t, true)
		cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: tlsServer.srv.URL, CA: tlsServer.caPEM()}}}
		got, _ := completeAuthProvider(t, cfg, nil, "auth", "login", "prod")
		assert.Equal(t, authConfigNames, got, "the entry's own CA")

		got, _ = completeAuthProvider(t, &config.Config{}, nil, "auth", "login", "--url", tlsServer.srv.URL, "--cacert", tlsServer.caFile(t))
		assert.Equal(t, authConfigNames, got, "--cacert with --url")

		got, _ = completeAuthProvider(t, &config.Config{}, nil, "auth", "login", "--url", tlsServer.srv.URL)
		assert.Empty(t, got, "no CA: the certificate is not trusted")
	})
	t.Run("RANCHER_CLI_CACERT for the entry named", func(t *testing.T) {
		tlsServer := newAuthListServer(t, true)
		cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: tlsServer.srv.URL}}}
		got, _ := completeAuthProvider(t, cfg, map[string]string{config.EnvCACert: tlsServer.caFile(t)}, "auth", "login", "prod")
		assert.Equal(t, authConfigNames, got, "the variable's CA, as the login itself uses it for an entry with none saved")
	})
}

// TestLegacyTokenCompletesAuthProviderFromItsOwnTarget asserts the legacy
// token command completes --auth-provider from the --server it requires and
// never from the current server entry, trusts --cacerts, and offers nothing
// with --skip-verify.
// NOT parallel: the environment and the stored config are process-wide.
func TestLegacyTokenCompletesAuthProviderFromItsOwnTarget(t *testing.T) {
	t.Run("--server", func(t *testing.T) {
		current, named := newAuthListServer(t, false), newAuthListServer(t, false)
		got, _ := completeAuthProvider(t, &config.Config{CurrentServer: "cur", Servers: map[string]config.Server{"cur": {URL: current.srv.URL}}}, nil,
			"token", "--server", named.srv.URL, "--user", "alice")
		assert.Equal(t, authConfigNames, got)
		assert.Equal(t, []string{"/v1-public/authproviders"}, named.served())
		assert.Empty(t, current.served())
	})
	t.Run("no --server gives nothing even with a current entry", func(t *testing.T) {
		current := newAuthListServer(t, false)
		got, _ := completeAuthProvider(t, &config.Config{CurrentServer: "cur", Servers: map[string]config.Server{"cur": {URL: current.srv.URL}}}, nil, "token", "--user", "alice")
		assert.Empty(t, got)
		assert.Empty(t, current.served())
	})
	t.Run("--cacerts", func(t *testing.T) {
		tlsServer := newAuthListServer(t, true)
		got, _ := completeAuthProvider(t, &config.Config{}, nil, "token", "--server", tlsServer.srv.URL, "--user", "alice", "--cacerts", tlsServer.caFile(t))
		assert.Equal(t, authConfigNames, got)
		got, _ = completeAuthProvider(t, &config.Config{}, nil, "token", "--server", tlsServer.srv.URL, "--user", "alice")
		assert.Empty(t, got, "no CA: the certificate is not trusted")
	})
	t.Run("--skip-verify", func(t *testing.T) {
		named := newAuthListServer(t, false)
		for _, words := range [][]string{
			{"token", "--skip-verify", "--server", named.srv.URL, "--user", "alice"},
			{"token", "--server", named.srv.URL, "--user", "alice", "--skip-verify"},
		} {
			got, _ := completeAuthProvider(t, &config.Config{}, nil, words...)
			assert.Empty(t, got, "%v", words)
		}
		assert.Empty(t, named.served())
	})
}

// fakeTarget is a command with the real source and a CompletionTarget that
// answers as configured.
type fakeTarget struct {
	server, ca string
	ok         bool
}

func (fakeTarget) LiveSource() completion.Source { return tokencomplete.AuthConfigSource() }

func (f fakeTarget) CompletionTarget(*config.Config, completion.Request) (string, string, bool) {
	return f.server, f.ca, f.ok
}

// authProviderSite is the --auth-provider flag-value site of a command.
func authProviderSite() CompletionSite {
	return CompletionSite{Kind: SiteFlagValue, FlagName: "auth-provider", FlagSpelling: "--auth-provider"}
}

func storeCompletionConfig(t *testing.T) {
	t.Helper()
	setStoredConfigForTest(t, &config.Config{Completion: config.CompletionConfig{Network: true}}, t.TempDir())
	for _, name := range []string{config.EnvToken, config.EnvURL, config.EnvCACert} {
		t.Setenv(name, "")
	}
}

func authCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestANoCredentialSourceSendsNoBearer asserts the surface attaches no token
// to the request of a source that needs none, even with a management token
// in the environment, and needs no credential to answer.
// NOT parallel: the environment and the stored config are process-wide.
func TestANoCredentialSourceSendsNoBearer(t *testing.T) {
	s := newAuthListServer(t, false)
	d := runtime.Descriptor{Name: "auth login", Flags: []runtime.ArgSpec{{Name: "auth-provider", Type: "string"}}}

	storeCompletionConfig(t)
	got := liveCandidates(authCtx(t), authProviderSite(), d, fakeTarget{server: s.srv.URL, ok: true}, []string{"--auth-provider"})
	assert.Equal(t, authConfigNames, sorted(got), "no credential anywhere")

	t.Setenv(config.EnvToken, "mgmt-token")
	got = liveCandidates(authCtx(t), authProviderSite(), d, fakeTarget{server: s.srv.URL, ok: true}, []string{"--auth-provider"})
	assert.Equal(t, authConfigNames, sorted(got))
	assert.Equal(t, []string{"", ""}, s.authorizations(), "no Authorization header on either request")
}

// TestANoCredentialTargetIsCheckedLikeAnyServerURL asserts a target the
// command returns with a scheme other than http or https is refused before
// any request, and one with a trailing slash is requested once, at the
// provider list's own path, from its canonical form.
// NOT parallel: the environment and the stored config are process-wide.
func TestANoCredentialTargetIsCheckedLikeAnyServerURL(t *testing.T) {
	s := newAuthListServer(t, false)
	d := runtime.Descriptor{Name: "auth login", Flags: []runtime.ArgSpec{{Name: "auth-provider", Type: "string"}}}
	storeCompletionConfig(t)
	t.Setenv(config.EnvCompletionDebug, "1")
	var trace strings.Builder
	prev, prevOut := rawCompleting, completionDebugOut
	rawCompleting, completionDebugOut = true, &trace
	t.Cleanup(func() { rawCompleting, completionDebugOut = prev, prevOut })

	got := liveCandidates(authCtx(t), authProviderSite(), d, fakeTarget{server: "ftp://" + strings.TrimPrefix(s.srv.URL, "http://"), ok: true}, []string{"--auth-provider"})
	assert.Empty(t, got)
	assert.Empty(t, s.served())
	assert.Contains(t, trace.String(), "stop: server URL scheme unsupported")

	got = liveCandidates(authCtx(t), authProviderSite(), d, fakeTarget{server: s.srv.URL + "/", ok: true}, []string{"--auth-provider"})
	assert.Equal(t, authConfigNames, sorted(got))
	assert.Equal(t, []string{"/v1-public/authproviders"}, s.served(), "one request, from the canonical server")
}

// TestANoCredentialSourceOffersNothingWhenItCannotAsk asserts no suggestions
// and no request when the command names no server, when the request fails,
// and when the command has no CompletionTarget at all.
// NOT parallel: the environment and the stored config are process-wide.
func TestANoCredentialSourceOffersNothingWhenItCannotAsk(t *testing.T) {
	d := runtime.Descriptor{Name: "auth login", Flags: []runtime.ArgSpec{{Name: "auth-provider", Type: "string"}}}
	storeCompletionConfig(t)
	tail := []string{"--auth-provider"}

	s := newAuthListServer(t, false)
	assert.Empty(t, liveCandidates(authCtx(t), authProviderSite(), d, fakeTarget{server: s.srv.URL}, tail), "the command names no server")
	assert.Empty(t, s.served())

	assert.Empty(t, liveCandidates(authCtx(t), authProviderSite(), d, noTargetCompleter{}, tail), "no CompletionTarget")
	assert.Empty(t, s.served())

	failing := newAuthListServer(t, false)
	failing.status = http.StatusInternalServerError
	assert.Empty(t, liveCandidates(authCtx(t), authProviderSite(), d, fakeTarget{server: failing.srv.URL, ok: true}, tail), "a failed request")
	assert.Equal(t, []string{"/v1-public/authproviders"}, failing.served())
}

// noTargetCompleter owns the source and has no CompletionTarget.
type noTargetCompleter struct{}

func (noTargetCompleter) LiveSource() completion.Source { return tokencomplete.AuthConfigSource() }

// TestACommandNeedingNoCredentialNeedsACompletionTarget asserts the build
// refuses a command whose source needs no credential on one of its sites and
// has no CompletionTarget, and accepts it with one or with a source that
// needs a credential.
func TestACommandNeedingNoCredentialNeedsACompletionTarget(t *testing.T) {
	t.Parallel()
	leaves := map[string]runtime.Descriptor{"x login": {Name: "x login", Flags: []runtime.ArgSpec{{Name: "auth-provider", Type: "string"}}}}
	completers := map[string]completion.Source{"x login": tokencomplete.AuthConfigSource()}

	err := validateCompletionTargets(leaves, completers, map[string]bool{})
	require.ErrorContains(t, err, "command x login has a completion source that needs no credential, but the command has no completion target (the server to connect to)")
	require.NoError(t, validateCompletionTargets(leaves, completers, map[string]bool{"x login": true}))

	managed := map[string]completion.Source{"x login": {
		Claim:    func(completion.Request) (completion.Requirements, bool) { return completion.ManagementListing(), true },
		Complete: tokencomplete.AuthConfigSource().Complete,
	}}
	require.NoError(t, validateCompletionTargets(leaves, managed, map[string]bool{}), "a source that needs a credential needs no target")
}

// TestTheSignInCommandsHaveCompletionTargets asserts the three commands that
// take --auth-provider own the live source and the target of their
// completion, so the build, which refuses a command without one, accepts
// them.
func TestTheSignInCommandsHaveCompletionTargets(t *testing.T) {
	t.Parallel()
	_, err := BuildRoot("test")
	require.NoError(t, err)
	found := map[string]bool{}
	for _, e := range buildEntries() {
		probe := e.make(runDeps{})
		name := probe.Describe().Name
		if name != "auth login" && name != "auth get-token" && name != "token" {
			continue
		}
		_, isCompleter := probe.(completion.Completer)
		_, hasTarget := probe.(completionTargeter)
		assert.True(t, isCompleter, name)
		assert.True(t, hasTarget, name)
		found[name] = true
	}
	assert.Len(t, found, 3)
}
