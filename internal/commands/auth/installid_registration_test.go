package auth

import (
	"bytes"
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/commands/register"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

const (
	installIDRoute = "/v1/management.cattle.io.settings/install-uuid"
	installIDValue = "d3b07384-d9a0-4e7a-9c2b-5f6a7b8c9d0e"
)

// installIDServer is a TLS server that answers the version request and the
// install id request, keeps the Authorization header of each install id
// request, and hands every other path to next (404 when there is none). Its
// certificate can be swapped after a login, to give the registered request a
// server the login's CA did not sign.
type installIDServer struct {
	*httptest.Server
	cert atomic.Pointer[tls.Certificate]
	mu   sync.Mutex
	auth []string
}

func newInstallIDServer(t *testing.T, fix tlsFixture, next http.Handler) *installIDServer {
	t.Helper()
	s := &installIDServer{}
	s.cert.Store(&fix.cert)
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/rancherversion":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Version":"v2.20.0"}`))
		case r.URL.Path == installIDRoute:
			s.mu.Lock()
			s.auth = append(s.auth, r.Header.Get("Authorization"))
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"value":"` + installIDValue + `"}`))
		case next != nil:
			next.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	s.Listener = tls.NewListener(s.Listener, &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return s.cert.Load(), nil },
	})
	s.Start()
	s.URL = "https://" + s.Listener.Addr().String()
	t.Cleanup(s.Close)
	return s
}

// swapCertificate presents fix from now on and closes the open connections;
// the next request makes a new handshake.
func (s *installIDServer) swapCertificate(fix tlsFixture) {
	s.cert.Store(&fix.cert)
	s.CloseClientConnections()
}

func (s *installIDServer) installIDRequests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auth...)
}

// flowServerWith is flowServer for a facade the caller configures: a TLS
// Rancher whose certificate only the returned CA trusts, with the install id
// route in front of the facade.
func flowServerWith(t *testing.T, opts testrancher.Options) (*installIDServer, tlsFixture) {
	t.Helper()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	return newInstallIDServer(t, fix, testrancher.New(opts)), fix
}

// pastedServer is a TLS server for the paste rows, with its CA.
func pastedServer(t *testing.T) (*installIDServer, tlsFixture) {
	t.Helper()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	return newInstallIDServer(t, fix, nil), fix
}

func acceptingVerify(context.Context, config.Effective) (string, error) { return "u-owner", nil }

// clearFetcherRecord empties the process-wide record before and after a test
// that reads it; such a test never runs in parallel.
func clearFetcherRecord(t *testing.T) {
	t.Helper()
	httpx.ClearRecords()
	t.Cleanup(httpx.ClearRecords)
}

// requireRegistered reads the registered fetcher and asserts its origin and
// skip, then sends the request and returns the Authorization header the
// server saw.
func requireRegistered(t *testing.T, srv *installIDServer, skip bool) string {
	t.Helper()
	f, ok := httpx.RegisteredInstallIDFetcher()
	require.True(t, ok, "a fetcher is registered")
	assert.Equal(t, kube.BaseURL(srv.URL), f.Origin)
	assert.Equal(t, skip, f.SkipFailureWait)
	before := len(srv.installIDRequests())
	value, status, err := f.Fetch(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 200, status)
	assert.Equal(t, installIDValue, value)
	seen := srv.installIDRequests()
	require.Len(t, seen, before+1)
	return seen[before]
}

// requireNotTrustedWithAnotherCertificate swaps the server's certificate for
// one the CA of the login did not sign and asserts the registered request
// fails: the fetcher's client never skips verification.
func requireNotTrustedWithAnotherCertificate(t *testing.T, srv *installIDServer) {
	t.Helper()
	srv.swapCertificate(selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	f, ok := httpx.RegisteredInstallIDFetcher()
	require.True(t, ok)
	_, status, err := f.Fetch(t.Context())
	require.Error(t, err)
	assert.Zero(t, status)
}

func requireNothingRegistered(t *testing.T) {
	t.Helper()
	_, ok := httpx.RegisteredInstallIDFetcher()
	assert.False(t, ok, "no fetcher is registered")
}

func pastedLoginCmd(dir string, store config.CredStore) *loginCmd {
	cmd := tlsLoginCmd(dir, store)
	cmd.verify = acceptingVerify
	return cmd
}

func menuPrompter(lines ...string) *ui.Prompter {
	return ui.New(&bytes.Buffer{}, newLineReader(lines...), false, true)
}

// TestLoginRegistersTheStoredPastedTokenAfterTheCheck asserts a pasted token
// the verify call accepted and the store took is registered: for the plain
// path and for --url with --cacert as text and as a file, with the skip on,
// the origin of the login's server and the bearer of the token it stored; the
// registered request fails for a server whose certificate the login's CA did
// not sign.
func TestLoginRegistersTheStoredPastedTokenAfterTheCheck(t *testing.T) {
	clearFetcherRecord(t)
	rows := []struct {
		name  string
		setup func(t *testing.T, dir string, srv *installIDServer, fix tlsFixture) cmdtest.Input
	}{
		{"the plain path of a stored entry", func(t *testing.T, dir string, srv *installIDServer, fix tlsFixture) cmdtest.Input {
			t.Helper()
			seedServer(t, dir, "prod", srv.URL, fix.pem, false)
			return plainLoginInput("prod")
		}},
		{"--url with --cacert as text", func(t *testing.T, dir string, srv *installIDServer, fix tlsFixture) cmdtest.Input {
			t.Helper()
			in := urlLoginInput("prod", srv.URL+"/")
			in.Str["cacert"] = fix.pem
			return in
		}},
		{"--url with --cacert as a file", func(t *testing.T, dir string, srv *installIDServer, fix tlsFixture) cmdtest.Input {
			t.Helper()
			in := urlLoginInput("prod", srv.URL)
			in.Str["cacert"] = cmdtest.WriteCAFile(t, fix.pem)
			return in
		}},
		{"the CA from the variable", func(t *testing.T, dir string, srv *installIDServer, fix tlsFixture) cmdtest.Input {
			t.Helper()
			t.Setenv(config.EnvCACert, fix.pem)
			return urlLoginInput("prod", srv.URL)
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			httpx.ClearRecords()
			srv, fix := pastedServer(t)
			dir, store := cmdtest.FilePolicyDir(t)
			in := row.setup(t, dir, srv, fix)
			_, err := pastedLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:secret\n"))
			require.NoError(t, err)
			assert.Equal(t, "Bearer id:secret", requireRegistered(t, srv, true))
			requireNotTrustedWithAnotherCertificate(t, srv)
		})
	}
}

// TestTheRegistrationAddsNothingToTheLoginOutput asserts a login that registers
// the install id request reports no warning and shows only its headline.
func TestTheRegistrationAddsNothingToTheLoginOutput(t *testing.T) {
	clearFetcherRecord(t)
	srv, fix := pastedServer(t)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, fix.pem, true)
	res, err := pastedLoginCmd(dir, store).Execute(t.Context(), plainLoginInput("prod"), cmdtest.StdinPrompter("id:secret\n"))
	require.NoError(t, err)
	_, ok := httpx.RegisteredInstallIDFetcher()
	require.True(t, ok, "the login registered the request")
	assert.Empty(t, res.Warnings)
	assert.Equal(t, "logged in to prod ("+config.DisplayServerURL(srv.URL)+")", strings.TrimSpace(res.Text))
}

// TestLoginRegistersTheTokenItStoredNotTheOneTheEntryHeld asserts a login
// that replaces a stored token registers the new one.
func TestLoginRegistersTheTokenItStoredNotTheOneTheEntryHeld(t *testing.T) {
	clearFetcherRecord(t)
	srv, fix := pastedServer(t)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, fix.pem, false)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "old:token"))

	_, err := pastedLoginCmd(dir, store).Execute(t.Context(), plainLoginInput("prod"), cmdtest.StdinPrompter("new:token\n"))
	require.NoError(t, err)
	assert.Equal(t, "Bearer new:token", requireRegistered(t, srv, true))
	stored, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "new:token", stored)
}

// TestLoginRegistersThroughACAAcceptedDuringTheRun asserts a guided-trust
// certificate the operator accepted in this run is the CA of the registered
// request.
func TestLoginRegistersThroughACAAcceptedDuringTheRun(t *testing.T) {
	clearFetcherRecord(t)
	srv, _ := pastedServer(t)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)

	p := ui.New(&bytes.Buffer{}, newLineReader("y", "id:sec"), false, true)
	_, err := pastedLoginCmd(dir, store).Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"token-stdin": true}, Pos: []string{"prod"}}, p)
	require.NoError(t, err)
	assert.Equal(t, "Bearer id:sec", requireRegistered(t, srv, true))
	requireNotTrustedWithAnotherCertificate(t, srv)
}

// TestLoginRegistersTheTokenAProviderIssued asserts a provider sign-in
// registers the token it stored: the derived token, the session token on a
// server without ext tokens, and the token of a sign-in picked at the menu
// with --no-verify typed.
func TestLoginRegistersTheTokenAProviderIssued(t *testing.T) {
	clearFetcherRecord(t)
	rows := []struct {
		name   string
		opts   testrancher.Options
		in     cmdtest.Input
		prompt func() *ui.Prompter
	}{
		{
			"the derived token",
			testrancher.Options{},
			cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}},
			func() *ui.Prompter { return menuPrompter("e2e-user", "e2e-pass") },
		},
		{
			"the session token of a server without ext tokens",
			testrancher.Options{Legacy: true},
			cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}},
			func() *ui.Prompter { return menuPrompter("e2e-user", "e2e-pass") },
		},
		{
			"menu answer 2 with --no-verify",
			testrancher.Options{},
			cmdtest.Input{Flag: map[string]bool{"no-verify": true}, Pos: []string{"prod"}},
			func() *ui.Prompter { return menuPrompter("2", "e2e-user", "e2e-pass") },
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			httpx.ClearRecords()
			srv, fix := flowServerWith(t, row.opts)
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", srv.URL, fix.pem, false)
			cmd := &loginCmd{
				dir:          dir,
				newStore:     func(string, string) (config.CredStore, string, error) { return store, "", nil },
				tlsPreflight: register.TLSPreflight,
				verify:       acceptingVerify,
			}
			_, err := cmd.Execute(t.Context(), row.in, row.prompt())
			require.NoError(t, err)
			stored, err := store.GetToken(config.ServerRef{Name: "prod"})
			require.NoError(t, err)
			require.NotEmpty(t, stored)
			assert.Equal(t, "Bearer "+stored, requireRegistered(t, srv, true))
			requireNotTrustedWithAnotherCertificate(t, srv)
		})
	}
}

// TestLoginRegistersNothingForATokenNoCheckAccepted asserts the paths that
// store a token nothing checked, or no token at all, register no fetcher: a
// pasted token with --no-verify (typed, and at the menu answer 1), a pasted
// token with no verify function, a failed verify, a store that refused a
// pasted or provider-issued token and a commit the drift check refused, and a pasted token with
// --no-verify on an http entry.
func TestLoginRegistersNothingForATokenNoCheckAccepted(t *testing.T) {
	clearFetcherRecord(t)
	rows := []struct {
		name string
		run  func(t *testing.T, srv *installIDServer, fix tlsFixture) error
	}{
		{"--no-verify with a piped token", func(t *testing.T, srv *installIDServer, fix tlsFixture) error {
			t.Helper()
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", srv.URL, fix.pem, false)
			in := plainLoginInput("prod")
			in.Flag["no-verify"] = true
			_, err := pastedLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:secret\n"))
			return err
		}},
		{"--no-verify with the paste menu answer 1", func(t *testing.T, srv *installIDServer, fix tlsFixture) error {
			t.Helper()
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", srv.URL, fix.pem, false)
			in := cmdtest.Input{Flag: map[string]bool{"no-verify": true}, Pos: []string{"prod"}}
			_, err := pastedLoginCmd(dir, store).Execute(t.Context(), in, menuPrompter("1", "id:secret"))
			return err
		}},
		{"no verify function and no --no-verify", func(t *testing.T, srv *installIDServer, fix tlsFixture) error {
			t.Helper()
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", srv.URL, fix.pem, false)
			_, err := tlsLoginCmd(dir, store).Execute(t.Context(), plainLoginInput("prod"), cmdtest.StdinPrompter("id:secret\n"))
			return err
		}},
		{"a verify call that failed", func(t *testing.T, srv *installIDServer, fix tlsFixture) error {
			t.Helper()
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", srv.URL, fix.pem, false)
			cmd := tlsLoginCmd(dir, store)
			cmd.verify = func(context.Context, config.Effective) (string, error) {
				return "", clierr.New(clierr.CodeAuth, "bad token")
			}
			_, err := cmd.Execute(t.Context(), plainLoginInput("prod"), cmdtest.StdinPrompter("id:secret\n"))
			require.Error(t, err)
			return nil
		}},
		{"a store that refused the token", func(t *testing.T, srv *installIDServer, fix tlsFixture) error {
			t.Helper()
			dir, _ := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", srv.URL, fix.pem, false)
			cmd := pastedLoginCmd(dir, unsupportedStore{})
			_, err := cmd.Execute(t.Context(), plainLoginInput("prod"), cmdtest.StdinPrompter("id:secret\n"))
			return err
		}},
		{"a commit the drift check refused", func(t *testing.T, srv *installIDServer, fix tlsFixture) error {
			t.Helper()
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", srv.URL, fix.pem, false)
			cmd := pastedLoginCmd(dir, store)
			cmd.beforeCommitHook = func() {
				cfg, _ := config.Load(config.FilePath(dir))
				cfg.Servers["prod"] = config.Server{URL: srv.URL + "/changed", CA: fix.pem}
				_ = cfg.Write()
			}
			_, err := cmd.Execute(t.Context(), plainLoginInput("prod"), cmdtest.StdinPrompter("id:secret\n"))
			require.Error(t, err)
			return nil
		}},
		{"a provider token the store refused", func(t *testing.T, srv *installIDServer, fix tlsFixture) error {
			t.Helper()
			flow, flowFix := flowServerWith(t, testrancher.Options{})
			dir, _ := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", flow.URL, flowFix.pem, false)
			cmd := &loginCmd{
				dir:          dir,
				newStore:     func(string, string) (config.CredStore, string, error) { return unsupportedStore{}, "", nil },
				tlsPreflight: register.TLSPreflight,
				verify:       acceptingVerify,
			}
			in := cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}}
			_, err := cmd.Execute(t.Context(), in, menuPrompter("e2e-user", "e2e-pass"))
			return err
		}},
		{"--no-verify on an http entry", func(t *testing.T, srv *installIDServer, fix tlsFixture) error {
			t.Helper()
			plain := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(plain.Close)
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "dev", plain.URL, "", false)
			in := plainLoginInput("dev")
			in.Flag["no-verify"] = true
			_, err := pastedLoginCmd(dir, store).Execute(t.Context(), in, cmdtest.StdinPrompter("id:secret\n"))
			return err
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			httpx.ClearRecords()
			srv, fix := pastedServer(t)
			require.NoError(t, row.run(t, srv, fix))
			requireNothingRegistered(t)
		})
	}
}

// authedCheck builds the status command for a server entry with a stored
// token, the connectivity check injected.
func authedCheck(t *testing.T, url, ca string, preflight func(context.Context, config.Effective) (string, error)) *authCheckCmd {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.Servers = map[string]config.Server{"default": {URL: url, CA: ca}}
	cfg.CurrentServer = "default"
	require.NoError(t, cfg.Write())
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "default"}, "stored:token"))
	return &authCheckCmd{dir: dir, store: store, preflight: preflight}
}

func passingPreflight(context.Context, config.Effective) (string, error) { return "v1.31.0", nil }

// TestAuthStatusRegistersTheTokenItCheckedWithoutTheSkip asserts a status
// run whose connectivity check passed registers the stored token, with the
// origin of the server and the skip off; the CA may be the entry's text, or
// a file named by the variable; a token in the variable replaces the stored
// one; probes that only warn never stop the registration.
func TestAuthStatusRegistersTheTokenItCheckedWithoutTheSkip(t *testing.T) {
	clearFetcherRecord(t)
	warning := func(context.Context, config.Effective) (string, error) {
		return "", clierr.New(clierr.CodeGeneric, "ext.cattle.io not served")
	}
	rows := []struct {
		name string
		env  func(t *testing.T, fix tlsFixture)
		ca   func(fix tlsFixture) string
		want string
	}{
		{"the entry's CA as text", func(*testing.T, tlsFixture) {}, func(f tlsFixture) string { return f.pem }, "Bearer stored:token"},
		{"the variable's CA as a file", func(t *testing.T, f tlsFixture) {
			t.Helper()
			t.Setenv(config.EnvCACert, cmdtest.WriteCAFile(t, f.pem))
		}, func(tlsFixture) string { return "" }, "Bearer stored:token"},
		{"the variable's token over the stored one", func(t *testing.T, f tlsFixture) {
			t.Helper()
			t.Setenv(config.EnvToken, "env:token")
		}, func(f tlsFixture) string { return f.pem }, "Bearer env:token"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			httpx.ClearRecords()
			srv, fix := pastedServer(t)
			row.env(t, fix)
			cmd := authedCheck(t, srv.URL, row.ca(fix), passingPreflight)
			cmd.identity, cmd.rancherVersion = warning, warning
			_, err := cmd.Execute(t.Context(), cmdtest.Input{}, ui.Discard())
			require.NoError(t, err)
			assert.Equal(t, row.want, requireRegistered(t, srv, false))
			requireNotTrustedWithAnotherCertificate(t, srv)
		})
	}
}

// TestAuthStatusRegistersNothingWhenTheCheckFailsOrThereIsNoServer asserts a
// failed connectivity check and a run with no current server register no
// fetcher.
func TestAuthStatusRegistersNothingWhenTheCheckFailsOrThereIsNoServer(t *testing.T) {
	clearFetcherRecord(t)
	t.Run("the connectivity check fails", func(t *testing.T) {
		httpx.ClearRecords()
		srv, fix := pastedServer(t)
		cmd := authedCheck(t, srv.URL, fix.pem, func(context.Context, config.Effective) (string, error) {
			return "", clierr.New(clierr.CodeAuth, "bad token")
		})
		_, err := cmd.Execute(t.Context(), cmdtest.Input{}, ui.Discard())
		require.Error(t, err)
		requireNothingRegistered(t)
	})
	t.Run("no current server", func(t *testing.T) {
		httpx.ClearRecords()
		dir := t.TempDir()
		var called atomic.Bool
		cmd := &authCheckCmd{dir: dir, store: config.NewFileStore(dir), preflight: func(context.Context, config.Effective) (string, error) {
			called.Store(true)
			return "v1.31.0", nil
		}}
		_, err := cmd.Execute(t.Context(), cmdtest.Input{}, ui.Discard())
		require.Error(t, err)
		assert.False(t, called.Load())
		requireNothingRegistered(t)
	})
}

// TestAuthStatusRegistersNothingForACAThatCannotBeRead asserts a CA path that
// does not exist builds no client and registers nothing, and the command
// still passes its own check.
func TestAuthStatusRegistersNothingForACAThatCannotBeRead(t *testing.T) {
	clearFetcherRecord(t)
	cmd := authedCheck(t, "https://rancher.example.com", filepath.Join(t.TempDir(), "absent.pem"), passingPreflight)
	_, err := cmd.Execute(t.Context(), cmdtest.Input{}, ui.Discard())
	require.NoError(t, err)
	requireNothingRegistered(t)
}
