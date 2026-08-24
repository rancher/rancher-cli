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
// /rancherversion the way the real preflight and PS-8 probe both expect.
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

// tlsLoginCmd builds a loginCmd wired with the real TLS preflight and a
// stubbed version probe, so these tests exercise the actual preflight
// implementation against real TLS fixtures rather than the newLoginCmd
// test helper's nil-skip default.
func tlsLoginCmd(dir string, store config.CredStore) *loginCmd {
	return &loginCmd{
		dir:            dir,
		newStore:       func(string, string) (config.CredStore, string, error) { return store, "", nil },
		rancherVersion: stubRancherVersion,
		tlsPreflight:   tlsPreflightDefault,
	}
}

func TestLoginTokenStdinRejectsUntrustedCertBeforePersisting(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := filePolicyDir(t)
	cmd := tlsLoginCmd(dir, store)
	in := args{s: map[string]string{"server": srv.URL, "name": "prod"}, b: map[string]bool{"token-stdin": true}}
	_, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	details, ok := ce.Details.(tlstrust.Details)
	require.True(t, ok)
	assert.Equal(t, tlstrust.KindUnknownAuthority, details.Kind)
	assert.Contains(t, ce.Hint, "--cacert")
	assert.NotContains(t, ce.Hint, "-sk", "the hint must never suggest an insecure /cacerts fetch")

	cfg, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	_, exists := cfg.Servers["prod"]
	assert.False(t, exists, "an untrusted server must never reach config persistence")
	_, tokErr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, tokErr, "the token must never be persisted either")
}

// TestLoginPastedTokenRejectsUntrustedCertBeforePersisting pins the same gap
// for the interactive paste path, which — like token-stdin — makes no
// network call of its own before this preflight.
func TestLoginPastedTokenRejectsUntrustedCertBeforePersisting(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := filePolicyDir(t)
	cmd := tlsLoginCmd(dir, store)
	p := ui.New(&bytes.Buffer{}, newLineReader("1", "id:sec"), false, true)
	in := args{s: map[string]string{"server": srv.URL, "name": "prod"}}
	_, err := cmd.Execute(t.Context(), in, p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)

	_, tokErr := store.GetToken(config.ServerRef{Name: "prod"})
	assert.Error(t, tokErr)
}

// TestLoginTokenStdinSucceedsWithMatchingCacert proves the preflight accepts
// the same server once given the CA (here, the self-signed leaf itself) that
// verifies it, and login proceeds to persist as normal.
func TestLoginTokenStdinSucceedsWithMatchingCacert(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := filePolicyDir(t)
	cmd := tlsLoginCmd(dir, store)
	in := args{s: map[string]string{"server": srv.URL, "name": "prod", "cacert": fix.pem}, b: map[string]bool{"token-stdin": true}}
	res, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
	assert.Equal(t, "LoginResult", res.Kind)

	tok, terr := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, terr)
	assert.Equal(t, "id:sec", tok)
}

// TestLoginPreflightUsesStoredCAWithoutExplicitCacert pins the CA-precedence
// fix at the preflight seam: a nameless re-login with no --cacert must
// preflight against the ALREADY-STORED CA, never silently fall back to
// system roots (which would reject this self-signed server every time).
func TestLoginPreflightUsesStoredCAWithoutExplicitCacert(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := filePolicyDir(t)
	cmd := tlsLoginCmd(dir, store)
	first := args{s: map[string]string{"server": srv.URL, "name": "prod", "cacert": fix.pem}, b: map[string]bool{"token-stdin": true}}
	_, err := cmd.Execute(t.Context(), first, stdinPrompter("id:sec\n"))
	require.NoError(t, err)

	nameless := args{s: map[string]string{"server": srv.URL}, b: map[string]bool{"token-stdin": true}}
	_, err = cmd.Execute(t.Context(), nameless, stdinPrompter("id2:sec2\n"))
	require.NoError(t, err, "the stored CA must carry the preflight, not an empty --cacert")

	tok, terr := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, terr)
	assert.Equal(t, "id2:sec2", tok)
}

// TestLoginProviderFlowReloginUsesStoredCAWithoutCacert pins the same
// CA-precedence fix on the provider-auth path: before this fix,
// resolveProviderLoginToken resolved --cacert alone and ignored the entry's
// stored CA, so a same-server provider re-login without --cacert built a
// system-roots client and failed against this self-signed server every time.
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
			_, _ = w.Write([]byte(`{"token":"token-sess1:sessionsecret"}`))
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
	cmd := tlsLoginCmd(dir, store)
	named := args{s: map[string]string{"server": srv.URL, "name": "prod", "cacert": fix.pem}, b: map[string]bool{"token-stdin": true}}
	_, err := cmd.Execute(t.Context(), named, stdinPrompter("id:sec\n"))
	require.NoError(t, err)

	p := ui.New(&bytes.Buffer{}, newLineReader("alice", "secret"), false, true)
	relogin := args{s: map[string]string{"server": srv.URL, "name": "prod", "auth-provider": "localProvider"}}
	_, err = cmd.Execute(t.Context(), relogin, p)
	require.NoError(t, err, "provider re-login must reuse the stored CA, not rediscover trust from an empty --cacert")
}

// TestLoginExplicitCacertMissingPathIsHardLocalError pins the "explicit
// --cacert failure is never eligible for guided replacement" rule at its
// simplest edge: a path that does not exist fails locally, before any
// handshake, as VALIDATION.
func TestLoginExplicitCacertMissingPathIsHardLocalError(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	cmd := tlsLoginCmd(dir, store)
	in := args{s: map[string]string{
		"server": "https://r.ex", "name": "prod", "cacert": filepath.Join(t.TempDir(), "missing.pem"),
	}, b: map[string]bool{"token-stdin": true}}
	_, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, "--cacert")
}

// TestLoginStoredCacertMissingPathNamesEntry pins the stored-path arm of the
// same rule: once a CA path stored with an entry disappears from disk, a
// later re-login without --cacert is a hard local error naming the entry and
// the exact repair command — never a silent system-roots fallback and never
// a guided re-establishment prompt (ruled out in v1).
func TestLoginStoredCacertMissingPathNamesEntry(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, []byte(fix.pem), 0o600))

	dir, store := filePolicyDir(t)
	cmd := tlsLoginCmd(dir, store)
	first := args{s: map[string]string{"server": srv.URL, "name": "prod", "cacert": caPath}, b: map[string]bool{"token-stdin": true}}
	_, err := cmd.Execute(t.Context(), first, stdinPrompter("id:sec\n"))
	require.NoError(t, err)

	require.NoError(t, os.Remove(caPath))

	nameless := args{s: map[string]string{"server": srv.URL}, b: map[string]bool{"token-stdin": true}}
	_, err = cmd.Execute(t.Context(), nameless, stdinPrompter("id2:sec2\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Message, `"prod"`)
	assert.Contains(t, ce.Hint, "rancher config delete-server prod")
}

// TestLoginHostnameMismatchNeverSuggestsCacert pins the failure-taxonomy
// rule end to end: a certificate valid for a different name is a structured
// TLS_TRUST failure whose hint never suggests --cacert (pinning a CA cannot
// repair a hostname mismatch).
func TestLoginHostnameMismatchNeverSuggestsCacert(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "wronghost.example", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := filePolicyDir(t)
	cmd := tlsLoginCmd(dir, store)
	in := args{s: map[string]string{"server": srv.URL, "name": "prod", "cacert": fix.pem}, b: map[string]bool{"token-stdin": true}}
	_, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	details, ok := ce.Details.(tlstrust.Details)
	require.True(t, ok)
	assert.Equal(t, tlstrust.KindHostnameMismatch, details.Kind)
	assert.NotContains(t, ce.Hint, "obtain the server's CA", "must never suggest pinning a CA fixes a hostname mismatch")
}

// TestLoginExpiredCertNeverSuggestsCacert covers the second non-actionable
// kind: an expired certificate cannot be fixed by pinning a CA either.
func TestLoginExpiredCertNeverSuggestsCacert(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	srv := newTLSFake(t, fix)

	dir, store := filePolicyDir(t)
	cmd := tlsLoginCmd(dir, store)
	in := args{s: map[string]string{"server": srv.URL, "name": "prod", "cacert": fix.pem}, b: map[string]bool{"token-stdin": true}}
	_, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code)
	details, ok := ce.Details.(tlstrust.Details)
	require.True(t, ok)
	assert.Equal(t, tlstrust.KindExpired, details.Kind)
	assert.NotContains(t, ce.Hint, "obtain the server's CA", "must never suggest pinning a CA fixes an expired certificate")
}

// TestLoginPreflightSkipsHTTPServers pins the "every HTTPS login path"
// scope: an http:// server has no TLS to verify, so the preflight must be a
// no-op rather than attempting a handshake that was never going to happen.
func TestLoginPreflightSkipsHTTPServers(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	dir, store := filePolicyDir(t)
	cmd := tlsLoginCmd(dir, store)
	in := args{s: map[string]string{"server": srv.URL, "name": "prod"}, b: map[string]bool{"token-stdin": true}}
	_, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err)
}

// TestLoginPreflightUnreachableServerStillWarnsOnly pins that this
// preflight narrows to certificate trust only: an unreachable server (no
// TLS to classify at all) keeps today's warn-only posture rather than
// becoming a new hard failure mode for login.
func TestLoginPreflightUnreachableServerStillWarnsOnly(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	cmd := tlsLoginCmd(dir, store)
	in := args{s: map[string]string{"server": "https://127.0.0.1:1", "name": "prod"}, b: map[string]bool{"token-stdin": true}}
	_, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err, "an unreachable server must not become a preflight failure")
}

// TestLoginPreflightRunsBeforeTokenRead pins the ordering the minted-token
// safety depends on: a failed preflight must surface before any token is
// read or auth flow launched — with garbage on stdin, the TLS_TRUST error
// (not a token-shape error) must come back.
func TestLoginPreflightRunsBeforeTokenRead(t *testing.T) {
	t.Parallel()
	fix := selfSignedFixture(t, "127.0.0.1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	srv := newTLSFake(t, fix)
	dir, store := filePolicyDir(t)
	in := args{s: map[string]string{"server": srv.URL, "name": "prod"}, b: map[string]bool{"token-stdin": true}}
	_, err := tlsLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("not-a-token\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeTLSTrust, ce.Code, "the preflight must fire before token reading/validation")
}

// TestLoginConflictWhenStoredCAChangesDuringLogin pins the entry-plan
// binding: authentication ran under the planned entry's CA; if a concurrent
// writer replaces that CA before the locked write, the login conflicts
// instead of persisting credentials verified under different trust.
func TestLoginConflictWhenStoredCAChangesDuringLogin(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seed := args{s: map[string]string{"server": "https://r.ex", "name": "prod", "cacert": "-----BEGIN CERTIFICATE-----old"}, b: map[string]bool{"token-stdin": true}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), seed, stdinPrompter("id:sec\n"))
	require.NoError(t, err)

	// The preflight hook runs between the pre-auth plan and the locked
	// write — mutate the stored CA there to simulate the concurrent writer.
	cmd := &loginCmd{
		dir:            dir,
		newStore:       func(string, string) (config.CredStore, string, error) { return store, "", nil },
		rancherVersion: stubRancherVersion,
		tlsPreflight: func(context.Context, string, string) (string, error) {
			cfg, err := config.Load(config.FilePath(dir))
			if err != nil {
				return "", err
			}
			cfg.Servers["prod"] = config.Server{URL: "https://r.ex", CA: "-----BEGIN CERTIFICATE-----swapped"}
			return "", cfg.Write()
		},
	}
	relogin := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err = cmd.Execute(t.Context(), relogin, stdinPrompter("id2:sec2\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "-----BEGIN CERTIFICATE-----swapped", cfg.Servers["prod"].CA, "the concurrent writer's CA is untouched")
	tok, err := store.GetToken(config.ServerRef{Name: "prod"})
	require.NoError(t, err)
	assert.Equal(t, "id:sec", tok, "the conflicted login must not overwrite the stored token")
}

// TestLoginConflictWhenEntrySelectionDriftsDuringLogin: a concurrent writer
// inserting a lexicographically earlier alias for the same URL changes the
// nameless selection between plan and locked write — conflict, never a
// silent adoption of an entry authentication never ran against.
func TestLoginConflictWhenEntrySelectionDriftsDuringLogin(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seed := args{s: map[string]string{"server": "https://r.ex", "name": "prod"}, b: map[string]bool{"token-stdin": true}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), seed, stdinPrompter("id:sec\n"))
	require.NoError(t, err)

	cmd := &loginCmd{
		dir:            dir,
		newStore:       func(string, string) (config.CredStore, string, error) { return store, "", nil },
		rancherVersion: stubRancherVersion,
		tlsPreflight: func(context.Context, string, string) (string, error) {
			cfg, err := config.Load(config.FilePath(dir))
			if err != nil {
				return "", err
			}
			cfg.Servers["aaa"] = config.Server{URL: "https://r.ex"}
			return "", cfg.Write()
		},
	}
	relogin := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err = cmd.Execute(t.Context(), relogin, stdinPrompter("id2:sec2\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
}

// TestLoginPrecheckConfigLoadFailureAborts: the pre-auth config read feeds
// the trust decision (which stored CA backs the preflight) — a failed load
// must abort, never silently collapse a custom-CA server to system roots.
func TestLoginPrecheckConfigLoadFailureAborts(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("{not yaml"), 0o600))
	in := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Contains(t, ce.Message, "reading config")
}

// TestLoginConflictWhenEntryDeletedDuringLogin closes the explicit-name gap
// the plan-binding comment claims: a concurrently deleted entry must
// conflict, never be silently resurrected with the CA snapshotted before
// the deletion (which may be exactly why it was deleted).
func TestLoginConflictWhenEntryDeletedDuringLogin(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	seed := args{s: map[string]string{"server": "https://r.ex", "name": "prod", "cacert": "-----BEGIN CERTIFICATE-----doomed"}, b: map[string]bool{"token-stdin": true}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), seed, stdinPrompter("id:sec\n"))
	require.NoError(t, err)

	cmd := &loginCmd{
		dir:            dir,
		newStore:       func(string, string) (config.CredStore, string, error) { return store, "", nil },
		rancherVersion: stubRancherVersion,
		tlsPreflight: func(context.Context, string, string) (string, error) {
			cfg, err := config.Load(config.FilePath(dir))
			if err != nil {
				return "", err
			}
			delete(cfg.Servers, "prod")
			return "", cfg.Write()
		},
	}
	relogin := args{s: map[string]string{"server": "https://r.ex", "name": "prod"}, b: map[string]bool{"token-stdin": true}}
	_, err = cmd.Execute(t.Context(), relogin, stdinPrompter("id2:sec2\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Contains(t, ce.Message, "deleted")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.NotContains(t, cfg.Servers, "prod", "the deleted entry must not be resurrected")
}

// TestLoginHTTPServerIgnoresStaleStoredCAFile: CA resolution is https-only —
// a stored CA path that no longer exists must not break a login to an
// http:// server that never verifies a certificate.
func TestLoginHTTPServerIgnoresStaleStoredCAFile(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.Servers["dev"] = config.Server{URL: "http://r.ex", CA: "/no/such/ca.pem"}
	require.NoError(t, cfg.Write())

	in := args{s: map[string]string{"server": "http://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err = newLoginCmd(dir, store).Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	require.NoError(t, err, "an http login must never resolve (or trip over) a stored CA file")
}

// TestLoginConflictWhenStoredCAFileContentRotatesDuringLogin: the drift
// check must compare resolved bytes, not selector strings — a CA file
// rotated in place under the same path mid-login is trust drift the
// plan-binding contract promises to conflict on.
func TestLoginConflictWhenStoredCAFileContentRotatesDuringLogin(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	caPath := dir + "/rotating-ca.pem"
	require.NoError(t, os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----old"), 0o600))
	seed := args{s: map[string]string{"server": "https://r.ex", "name": "prod", "cacert": caPath}, b: map[string]bool{"token-stdin": true}}
	_, err := newLoginCmd(dir, store).Execute(t.Context(), seed, stdinPrompter("id:sec\n"))
	require.NoError(t, err)

	cmd := &loginCmd{
		dir:            dir,
		newStore:       func(string, string) (config.CredStore, string, error) { return store, "", nil },
		rancherVersion: stubRancherVersion,
		tlsPreflight: func(context.Context, string, string) (string, error) {
			return "", os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----rotated"), 0o600)
		},
	}
	relogin := args{s: map[string]string{"server": "https://r.ex"}, b: map[string]bool{"token-stdin": true}}
	_, err = cmd.Execute(t.Context(), relogin, stdinPrompter("id2:sec2\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Contains(t, ce.Message, "content")
}

// TestLoginConflictWhenExplicitCacertFileContentRotatesDuringLogin: the
// content-rotation guard applies to the explicit --cacert path exactly as
// to a stored one — the flag being explicit does not make mid-login file
// rotation any less of a trust drift.
func TestLoginConflictWhenExplicitCacertFileContentRotatesDuringLogin(t *testing.T) {
	t.Parallel()
	dir, store := filePolicyDir(t)
	caPath := dir + "/explicit-ca.pem"
	require.NoError(t, os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----old"), 0o600))
	cmd := &loginCmd{
		dir:            dir,
		newStore:       func(string, string) (config.CredStore, string, error) { return store, "", nil },
		rancherVersion: stubRancherVersion,
		tlsPreflight: func(context.Context, string, string) (string, error) {
			return "", os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----rotated"), 0o600)
		},
	}
	in := args{s: map[string]string{"server": "https://r.ex", "name": "prod", "cacert": caPath}, b: map[string]bool{"token-stdin": true}}
	_, err := cmd.Execute(t.Context(), in, stdinPrompter("id:sec\n"))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeConflict, ce.Code)
	assert.Contains(t, ce.Message, "content")
}
