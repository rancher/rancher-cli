package commands

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tlsFixture is a self-signed leaf certificate (dynamiclistener-CA shape:
// the leaf signs itself, no separate CA cert) valid for dns, plus its PEM
// encoding for use as --cacert.
type tlsFixture struct {
	cert tls.Certificate
	pem  string
}

func selfSignedFixture(t *testing.T, dns string, notBefore, notAfter time.Time) tlsFixture {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dns},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	// httptest servers listen on a literal IP; a DNSNames-only SAN for an
	// IP-shaped name fails hostname verification for the wrong reason (no IP
	// SANs) on some verifiers before trust is even considered, making the
	// intended failure kind platform-dependent. An IP SAN keeps the
	// certificate unambiguously valid for its host so trust/expiry is the
	// only thing under test; a real DNS-shaped name (the hostname-mismatch
	// fixture) still goes through DNSNames as normal.
	if ip := net.ParseIP(dns); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{dns}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return tlsFixture{
		cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv},
		pem:  string(certPEM),
	}
}

// newTLSFake starts an httptest TLS server presenting fix.cert, serving
// /rancherversion the way the real preflight and registration's min-version
// check both expect.
func newTLSFake(t *testing.T, fix tlsFixture) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rancherversion" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Version":"v2.20.0"}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{fix.cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// tlsLoginCmd builds a loginCmd wired with the real TLS preflight, so these
// tests exercise the actual preflight implementation against real TLS
// fixtures rather than newPlainLoginCmd's nil-skip default. It is used for
// PLAIN login against an already-registered entry: the preflight enforces
// already-pinned trust, it cannot change it.
func tlsLoginCmd(dir string, store config.CredStore) *loginCmd {
	return &loginCmd{
		dir:          dir,
		newStore:     func(string, string) (config.CredStore, string, error) { return store, "", nil },
		tlsPreflight: tlsPreflightDefault,
	}
}

func TestLoginPlainExistingEntryRejectsUntrustedCert(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	cmd := tlsLoginCmd(dir, store)
	in := args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	details, ok := ce.Details.(tlstrust.Details)
	require.True(t, ok)
	assert.Equal(t, tlstrust.KindUnknownAuthority, details.Kind)
	assert.Empty(t, ce.Hint, "the new CA path is unknown — remediation belongs in the body, never a hint")
	assert.Contains(t, ce.Message, "rancher config update-server 'prod' --cacert")

	_, tokErr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, tokErr, "the token must never be persisted")
}

// TestLoginPastedTokenRejectsUntrustedCertBeforePersisting pins the same
// gate for the interactive paste path (promptForToken), which — like
// --token-stdin — makes no network call of its own before this preflight.
func TestLoginPastedTokenRejectsUntrustedCertBeforePersisting(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	cmd := tlsLoginCmd(dir, store)
	p := ui.New(&bytes.Buffer{}, newLineReader("1", "id:sec"), false, true)
	_, err := cmd.Execute(t.Context(), args{a: []string{"prod"}}, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)

	_, tokErr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, tokErr, "the token must never be persisted")
}

func TestLoginPlainExistingEntryWithStoredCASucceeds(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, fix.pem, false)
	cmd := tlsLoginCmd(dir, store)
	in := args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	res, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Equal(t, "LoginResult", res.Kind)

	tok, terr := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, terr)
	assert.Equal(t, "id:sec", tok)
}

// TestLoginProviderFlowReloginUsesStoredCAWithoutCacert pins the same
// CA-precedence fix on the provider-auth path: a re-login against an
// existing entry must dial with the STORED CA (no --cacert flag exists on
// plain login at all now), never system roots.
func TestLoginProviderFlowReloginUsesStoredCAWithoutCacert(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1-public/authproviders"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"}]}`))
		case strings.HasSuffix(r.URL.Path, "/v1-public/login"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"token-sess1","token":"token-sess1:sessionsecret"}`))
		case r.URL.Path == "/rancherversion":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Version":"v2.20.0"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{fix.cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, fix.pem, false)
	cmd := tlsLoginCmd(dir, store)
	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	relogin := args{s: map[string]string{"auth-provider": "localProvider"}, a: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), relogin, p)
	require.NoError(t, err, "provider re-login must reuse the stored CA, never rediscover trust")
}

// TestLoginPlainStoredCAPathMissingNamesEntry pins the stored-path arm: once
// a CA path stored with an entry disappears from disk, plain login is a hard
// local error naming the entry and the exact repair command — never a
// silent system-roots fallback.
func TestLoginPlainStoredCAPathMissingNamesEntry(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://r.ex", filepath.Join(t.TempDir(), "missing.pem"), false)
	cmd := tlsLoginCmd(dir, store)
	in := args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, `"prod"`)
	assert.Contains(t, ce.Message, "rancher config delete-server 'prod'")
	assert.Empty(t, ce.Hint, "no name-appropriate single command exists — restore the file, or delete-server — so no hint is emitted")
}

func TestLoginHostnameMismatchNeverSuggestsCacert(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "wronghost.example", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, fix.pem, false)
	cmd := tlsLoginCmd(dir, store)
	in := args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	details, ok := ce.Details.(tlstrust.Details)
	require.True(t, ok)
	assert.Equal(t, tlstrust.KindHostnameMismatch, details.Kind)
	assert.NotContains(t, ce.Hint, "obtain the server's CA", "must never suggest pinning a CA fixes a hostname mismatch")
}

func TestLoginExpiredCertNeverSuggestsCacert(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, fix.pem, false)
	cmd := tlsLoginCmd(dir, store)
	in := args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	details, ok := ce.Details.(tlstrust.Details)
	require.True(t, ok)
	assert.Equal(t, tlstrust.KindExpired, details.Kind)
	assert.NotContains(t, ce.Hint, "obtain the server's CA", "must never suggest pinning a CA fixes an expired certificate")
}

// TestLoginPlainPreflightSkipsHTTPServers pins the "every HTTPS login path"
// scope: an http:// server has no TLS to verify, so the preflight must be a
// no-op rather than attempting a handshake that was never going to happen.
func TestLoginPlainPreflightSkipsHTTPServers(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	cmd := tlsLoginCmd(dir, store)
	in := args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
}

// TestLoginPlainPreflightUnreachableServerNeverFailsLogin pins that this
// preflight narrows to certificate trust only: an unreachable server (no
// TLS to classify at all) keeps a warn-only posture rather than becoming a
// new hard failure mode for login — for a --token-stdin login, which makes
// no other network call, this means login succeeds outright.
func TestLoginPlainPreflightUnreachableServerNeverFailsLogin(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", "https://127.0.0.1:1", "", false)
	cmd := tlsLoginCmd(dir, store)
	in := args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err, "an unreachable server must not become a preflight failure")
}

// TestLoginPlainPreflightRunsBeforeTokenRead pins the ordering the created-token
// safety depends on: a failed preflight must surface before any token is
// read or auth flow launched — with garbage on stdin, the TLS_TRUST error
// (not a token-shape error) must come back.
func TestLoginPlainPreflightRunsBeforeTokenRead(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", false)
	in := args{b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := tlsLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("not-a-token\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code, "the preflight must fire before token reading/validation")
}

// TestLoginHTTPServerIgnoresStaleStoredCAFile: CA resolution is https-only —
// a stored CA path that no longer exists must not break a login to an
// http:// server that never verifies a certificate.
func TestLoginHTTPServerIgnoresStaleStoredCAFile(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seedServer(t, dir, "dev", "http://r.ex", "/no/such/ca.pem", false)
	in := args{b: map[string]bool{"token-stdin": true}, a: []string{"dev"}}
	_, err := newPlainLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err, "an http login must never resolve (or trip over) a stored CA file")
}

// TestLoginConflictWhenExplicitCacertFileContentRotatesDuringDelegatedLogin
// pins the content-rotation guard on the DELEGATED (--url --cacert) path,
// exercised end to end through login rather than only at the register.go
// unit level: the flag being explicit does not make mid-registration file
// rotation any less of a trust drift.
func TestLoginConflictWhenExplicitCacertFileContentRotatesDuringDelegatedLogin(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	caPath := dir + "/explicit-ca.pem"
	require.NoError(t, os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----old"), 0o600))
	cmd := &loginCmd{
		dir:      dir,
		newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil },
		tlsPreflight: func(context.Context, string, string) (string, error) {
			return "", os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----rotated"), 0o600)
		},
	}
	in := args{s: map[string]string{"url": "https://r.ex", "cacert": caPath}, b: map[string]bool{"token-stdin": true}, a: []string{"prod"}}
	_, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)

	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.NotContains(t, cfg.Servers, "prod", "registration itself conflicted; no entry, no auth attempt")
}

// TestServerLabelMatchesOriginSpelling asserts that the preflight names the
// server the way the origin-attributed classifier does: a scheme-default
// port is dropped, any other port is kept, and an unusable URL falls back
// to its host, lowercased like the origin spelling.
func TestServerLabelMatchesOriginSpelling(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "rancher.example", serverLabel("https://rancher.example:443"))
	assert.Equal(t, "rancher.example:8443", serverLabel("https://rancher.example:8443"))
	assert.Equal(t, "rancher.example", serverLabel("https://RANCHER.example/"))
	assert.Equal(t, "[::1]:8443", serverLabel("https://[::1]:8443"))
	assert.Equal(t, "h", serverLabel("ftp://h"), "an unsupported scheme has no origin; the host is the fallback")
	assert.Equal(t, "rancher.example:99999", serverLabel("https://RANCHER.example:99999"), "the fallback lowercases like the origin spelling")
}
