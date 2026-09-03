package token

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type tin struct {
	s map[string]string
	b map[string]bool
	a []string
}

func (t tin) String(k string) string  { return t.s[k] }
func (t tin) Bool(k string) bool      { return t.b[k] }
func (t tin) Strings(string) []string { return nil }
func (t tin) IsSet(string) bool       { return false }

// selfSignedCAPEM returns a freshly generated self-signed certificate as PEM,
// for tests that need a --cacert value httpx.New accepts without any server
// ever being reached.
func selfSignedCAPEM(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "token-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// serverCAPEM returns srv's own presented certificate as PEM, for use as
// --cacert so a test can reach an httptest TLS server with verification ON.
func serverCAPEM(srv *httptest.Server) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}
func (t tin) Args() []string { return t.a }

func TestExecCredentialJSONShape(t *testing.T) {
	cred := newExecCredential("kubeconfig-u-abc:xyz", time.Time{})
	b, err := json.Marshal(cred)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "ExecCredential", got["kind"])
	assert.Equal(t, "client.authentication.k8s.io/v1beta1", got["apiVersion"])
	status := got["status"].(map[string]any)
	assert.Equal(t, "kubeconfig-u-abc:xyz", status["token"])
}

func TestDeleteTokenCommand(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cmd := NewDeleteTokenCommand(dir, store).(runtime.ReadCommand)

	t.Run("no name and no --all is validation error", func(t *testing.T) {
		_, err := cmd.Execute(t.Context(), tin{}, ui.Discard())
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	})

	t.Run("--all flushes the cache", func(t *testing.T) {
		require.NoError(t, store.SetCachedCred("acme/u/c", "tok", time.Now().Add(time.Hour), "", "", ""))
		res, err := cmd.Execute(t.Context(), tin{b: map[string]bool{"all": true}}, ui.Discard())
		require.NoError(t, err)
		assert.Equal(t, "all cached credentials deleted\n", res.Text)
		_, _, gerr := store.GetCachedCred("acme/u/c")
		assert.ErrorIs(t, gerr, config.ErrNoToken)
	})

	t.Run("named delete removes the entry", func(t *testing.T) {
		require.NoError(t, store.SetCachedCred("acme/u/c", "tok", time.Now().Add(time.Hour), "", "", ""))
		res, err := cmd.Execute(t.Context(), tin{a: []string{"acme/u/c"}}, ui.Discard())
		require.NoError(t, err)
		assert.Equal(t, "cached credential acme/u/c deleted\n", res.Text)
		_, _, gerr := store.GetCachedCred("acme/u/c")
		assert.ErrorIs(t, gerr, config.ErrNoToken)
	})

	t.Run("--all with a name is a validation error, not a silent flush", func(t *testing.T) {
		require.NoError(t, store.SetCachedCred("keep/u/c", "tok", time.Now().Add(time.Hour), "", "", ""))
		_, err := cmd.Execute(t.Context(), tin{b: map[string]bool{"all": true}, a: []string{"keep/u/c"}}, ui.Discard())
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		// the cache must be untouched — nothing deleted
		_, _, gerr := store.GetCachedCred("keep/u/c")
		require.NoError(t, gerr)
	})

	t.Run("bare name deletes every entry sharing that name", func(t *testing.T) {
		scoped := config.CacheKey("prod", "r.ex", "c-1")
		require.NoError(t, store.SetCachedCred("prod", "legacy", time.Now().Add(time.Hour), "", "", ""))
		require.NoError(t, store.SetCachedCred(scoped, "scoped", time.Now().Add(time.Hour), "prod", "r.ex", "c-1"))
		res, err := cmd.Execute(t.Context(), tin{a: []string{"prod"}}, ui.Discard())
		require.NoError(t, err)
		deleted := res.Data.(map[string]any)["deleted"].([]string)
		assert.ElementsMatch(t, []string{"prod", scoped}, deleted)
		_, _, gerr := store.GetCachedCred("prod")
		require.ErrorIs(t, gerr, config.ErrNoToken)
		_, _, gerr = store.GetCachedCred(scoped)
		assert.ErrorIs(t, gerr, config.ErrNoToken)
	})

	t.Run("unknown name is an idempotent success", func(t *testing.T) {
		res, err := cmd.Execute(t.Context(), tin{a: []string{"ghost"}}, ui.Discard())
		require.NoError(t, err)
		deleted := res.Data.(map[string]any)["deleted"].([]string)
		assert.Empty(t, deleted, "already-deleted / unknown names must be a no-op success")
		assert.Equal(t, "no matching cached credential\n", res.Text, "ShapeText requires non-empty Text even on the idempotent no-op path")
	})
}

func TestBasicAuthNonInteractiveIsAuthError(t *testing.T) {
	t.Parallel()
	input := &loginInput{server: "https://test.example.com", authProvider: "localProvider"}
	_, err := basicAuth(t.Context(), nil, input, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
}

func TestExecCredentialOmitsEmptyStatusToken(t *testing.T) {
	// Embedding (not a json:",inline" tag) is what promotes kind/apiVersion.
	cred := newExecCredential("", time.Time{})
	b, err := json.Marshal(cred)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "ExecCredential", got["kind"])
	status := got["status"].(map[string]any)
	_, hasTok := status["token"]
	assert.False(t, hasTok, "empty token must be omitted, not emitted as empty string")
}

func TestGetTokenServerRequired(t *testing.T) {
	dir := t.TempDir()
	cmd := NewTokenCommand(dir, config.NewFileStore(dir)).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(), tin{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestGetTokenNoClusterRejectsBeforeCacheProbe(t *testing.T) {
	// A still-valid cache entry keyed with an empty cluster component (written
	// before --cluster became required) must NOT be served: the native command
	// rejects a missing --cluster before any cache read — a token must never
	// be returned for a cluster the caller didn't name.
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetCachedCred(config.CacheKey("prod", "https://r.ex", ""), "cached-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", ""))
	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)

	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "https://r.ex", "name": "prod"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "--cluster is required", ce.Message)
}

func TestGetTokenCacheHitIgnoresBrokenConfig(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	key := config.CacheKey("prod", "https://r.ex", "c-1")
	require.NoError(t, store.SetCachedCred(key, "cached-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))
	// A broken config.yaml must not block a warm cache hit when the connection
	// is fully specified by flags (finding 3).
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("\t: not yaml ["), 0o600))

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "https://r.ex", "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "cached-tok", cred.Status.Token)
}

func TestGetTokenCacheKeyScopedByServer(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	// Same --name cached against two servers must not collide.
	require.NoError(t, store.SetCachedCred(config.CacheKey("prod", "https://r1.example.com", "c-1"), "tok-r1", time.Now().Add(time.Hour), "prod", "https://r1.example.com", "c-1"))
	require.NoError(t, store.SetCachedCred(config.CacheKey("prod", "https://r2.example.com", "c-1"), "tok-r2", time.Now().Add(time.Hour), "prod", "https://r2.example.com", "c-1"))
	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)

	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "https://r2.example.com", "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "tok-r2", cred.Status.Token, "get-token must return the token scoped to the requested server")
}

// TestGetTokenSchemelessFlagSkipsLegacyHostOnlyKey is the third-re-review
// repro: --server host (schemeless) must NOT serve a legacy host-only cache
// record whose scheme is unknowable. The fast path must apply the default-https
// normalization and then skip the probe when the result still does not yield a
// valid absolute canonical URL (or in this case: the normalized key is distinct
// from the legacy host-only key so the probe misses).
func TestGetTokenSchemelessFlagSkipsLegacyHostOnlyKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	// Seed a legacy host-only key (CanonicalServerIdentity("host") → "host").
	legacyKey := config.CacheKey("prod", config.CanonicalServerIdentity("host"), "c-1")
	require.NoError(t, store.SetCachedCred(legacyKey, "legacy-tok", time.Now().Add(time.Hour), "prod", "host", "c-1"))
	// Break config so the full path cannot proceed — if the fast path
	// erroneously returns "legacy-tok" we never reach this error.
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("\t: not yaml ["), 0o600))

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "host", "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	// The fast path must miss, causing the full path to fail on the broken
	// config — proving "legacy-tok" was not returned.
	require.Error(t, err, "schemeless --server must not serve a legacy host-only cache record")
}

// TestGetTokenSchemelessFlagHitsCurrentHTTPSKey is the positive companion: a
// schemeless --server host normalizes to https://host and DOES hit a current-
// format cache record seeded for https://host.
func TestGetTokenSchemelessFlagHitsCurrentHTTPSKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	// Seed a current-format key for https://host.
	httpsKey := config.CacheKey("prod", config.CanonicalServerIdentity("https://host"), "c-1")
	require.NoError(t, store.SetCachedCred(httpsKey, "https-tok", time.Now().Add(time.Hour), "prod", "https://host", "c-1"))
	// Break config to prove it is the fast path (not the full resolved path)
	// that serves the hit.
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("\t: not yaml ["), 0o600))

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "host", "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "https-tok", cred.Status.Token, "schemeless --server must hit the current https:// cache record")
}

// TestGetTokenMintsFromStoredLoginCredential is the exercise-bug-4 repro: on
// a cache miss, get-token must mint a cluster-scoped token from the stored
// login credential for the matching configured server BEFORE ever touching
// the interactive provider flow. authProviderHits stays at 0 to prove the
// interactive path was provably not invoked.
func TestGetTokenMintsFromStoredLoginCredential(t *testing.T) {
	var authProviderHits, mintHits int32
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1-public/authproviders"):
			atomic.AddInt32(&authProviderHits, 1)
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == MintTokensPath && r.Method == http.MethodPost:
			atomic.AddInt32(&mintHits, 1)
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"minted"},"status":{"bearerToken":"ext/minted:secret"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "ext/minted:secret", cred.Status.Token)
	assert.Equal(t, int32(1), atomic.LoadInt32(&mintHits))
	assert.Equal(t, "Bearer stored-login-token", gotAuth, "the mint request must bear the stored login token, not a fresh session token")
	assert.Equal(t, int32(0), atomic.LoadInt32(&authProviderHits), "the interactive provider flow must not run when the stored credential mints successfully")
}

// TestGetTokenFallsBackToInteractiveWhenMintRejected covers the "mint is
// rejected" half of the fallback: a stored credential exists but the mint
// call fails, so get-token must still fall through to the interactive flow
// (observed here as an authproviders hit) rather than surfacing the mint
// error directly.
// TestGetTokenLegacyFallbackCachesWithBoundedTTL is the round-3 repro for
// finding 5: a legacy (pre-ext.cattle.io) Rancher's stored-credential
// fallback must still be cached — with a bounded, conservative TTL, since
// the raw stored token carries no expiry of its own — rather than never
// (a zero expiry trips round-2's own no-zero-expiry-cache gate, forcing a
// full mint-then-404 round trip on every single invocation).
func TestGetTokenLegacyFallbackCachesWithBoundedTTL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // no ext.cattle.io on this Rancher at all
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "stored-login-token", cred.Status.Token)

	_, expiry, cerr := store.GetCachedCred(config.CacheKey("prod", config.CanonicalServerIdentity(srv.URL), "local"))
	require.NoError(t, cerr, "the legacy fallback must be cached, not treated as zero-expiry")
	assert.WithinDuration(t, time.Now().Add(legacyMintTTL), expiry, time.Minute)
}

func TestGetTokenFallsBackToInteractiveWhenMintRejected(t *testing.T) {
	var authProviderHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1-public/authproviders"):
			authProviderHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"pingProvider"}]}`))
		case r.URL.Path == MintTokensPath && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.Discard())
	// SAML under a non-interactive prompter fast-fails AUTH — the point here
	// is that the flow reached the provider dance at all.
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, int32(1), authProviderHits.Load(), "a rejected mint must fall through to the interactive flow")
}

// TestGetTokenMintTimeoutSurfacesAsGenericTransportError asserts a stalled
// mint request is bounded by httpx's client-level timeout rather than
// hanging forever, and that the resulting error keeps MintDerivedToken's
// existing classification: a *clierr.Error with CodeGeneric, the same shape
// any other transport failure at this call site already takes (a timeout is
// not errMintDenied, so it must NOT fall back to the interactive flow the
// way a rejected 401/403 mint does).
func TestGetTokenMintTimeoutSurfacesAsGenericTransportError(t *testing.T) {
	// Not t.Parallel(): shrinks the package-global httpx.DefaultTimeout.
	restore := httpx.SetDefaultTimeoutForTest(20 * time.Millisecond)
	defer restore()

	var authProviderHits atomic.Int32
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1-public/authproviders"):
			authProviderHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"pingProvider"}]}`))
		case r.URL.Path == MintTokensPath && r.Method == http.MethodPost:
			<-block
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	// close(block) before srv.Close(): Close waits for in-flight handlers to
	// return, and the mint handler is still parked on <-block after the
	// client gives up — closing the server first would deadlock.
	defer func() { close(block); srv.Close() }()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	start := time.Now()
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.Discard())
	elapsed := time.Since(start)

	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Equal(t, int32(0), authProviderHits.Load(), "a timeout is not errMintDenied and must not trigger the interactive fallback")
	assert.Less(t, elapsed, 5*time.Second, "the mint request must be bounded by the client timeout, not hang on the blocked handler")
}

// TestGetTokenFallsBackToInteractiveWithNoStoredCredential covers the "no
// stored credential exists" half of the fallback.
func TestGetTokenFallsBackToInteractiveWithNoStoredCredential(t *testing.T) {
	var authProviderHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			authProviderHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"pingProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "c-1"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, int32(1), authProviderHits.Load(), "no stored credential must fall through to the interactive flow")
}

// TestGetTokenCachesMintedExpiry is the round-2 repro for finding 1: the
// stored-credential mint's status.expiresAt must be propagated into the
// cached credential's real expiry, not silently dropped.
func TestGetTokenCachesMintedExpiry(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == MintTokensPath && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"metadata":{"name":"minted"},"status":{"bearerToken":"ext/minted:secret","expiresAt":%q}}`, expiresAt)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.Discard())
	require.NoError(t, err)

	_, gotExpiry, cerr := store.GetCachedCred(config.CacheKey("prod", config.CanonicalServerIdentity(srv.URL), "local"))
	require.NoError(t, cerr, "a token with a real expiry must be cached")
	assert.WithinDuration(t, time.Now().Add(time.Hour), gotExpiry, time.Minute)
}

// TestGetTokenSkipsCacheWhenMintCarriesNoExpiry is the round-2 repro for
// finding 1: a mint response with no expiresAt must never be cached — a
// zero expiry reads back as never-expiring (GetCachedCred only evicts a
// past, non-zero expiry), so caching it would serve a stale token forever.
func TestGetTokenSkipsCacheWhenMintCarriesNoExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == MintTokensPath && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"minted"},"status":{"bearerToken":"ext/minted:secret"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	var notices bytes.Buffer
	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.New(&notices, strings.NewReader(""), true, false))
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "ext/minted:secret", cred.Status.Token, "the token is still usable for this call")

	_, _, cerr := store.GetCachedCred(config.CacheKey("prod", config.CanonicalServerIdentity(srv.URL), "local"))
	require.ErrorIs(t, cerr, config.ErrNoToken, "a zero-expiry token must not be cached")
	assert.Contains(t, notices.String(), "not caching")
}

// TestZeroExpiryCachedWhenAllowed mirrors TestGetTokenSkipsCacheWhenMintCarriesNoExpiry
// but runs as the legacy shim (legacyShim, the field that opts into zero-expiry
// caching). The credential must be written to the store (zero expiry =
// never-expiring on the legacy path), and the emitted ExecCredential must
// carry no ExpirationTimestamp.
func TestZeroExpiryCachedWhenAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == MintTokensPath && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"minted"},"status":{"bearerToken":"ext/minted:secret"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := tokenCmd{dir: dir, store: store}
	res, err := cmd.runWithParams(t.Context(), runParams{
		Server:     srv.URL,
		Name:       "prod",
		legacyShim: true,
	}, ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "ext/minted:secret", cred.Status.Token)
	assert.Nil(t, cred.Status.ExpirationTimestamp, "zero expiry must not set ExpirationTimestamp")

	tok, exp, cerr := store.GetCachedCred(config.CacheKey("prod", config.CanonicalServerIdentity(srv.URL), ""))
	require.NoError(t, cerr, "zero-expiry credential must be cached on the legacy shim path")
	assert.Equal(t, "ext/minted:secret", tok)
	assert.True(t, exp.IsZero(), "cached expiry must be zero")
}

// TestGetTokenMintScopesToCluster is the round-2 repro for finding 2:
// --cluster must be threaded into the mint request as spec.clusterName, so
// two different --cluster values produce two distinct scoped mints.
func TestGetTokenMintScopesToCluster(t *testing.T) {
	var gotClusters []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == clustersPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"c-a"}},{"metadata":{"name":"c-b"}}]}`))
		case r.URL.Path == MintTokensPath && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			gotClusters = append(gotClusters, gjson.GetBytes(body, "spec.clusterName").String())
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"minted"},"status":{"bearerToken":"ext/minted:secret"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "cluster": "c-a"}},
		ui.Discard())
	require.NoError(t, err)
	_, err = cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "cluster": "c-b"}},
		ui.Discard())
	require.NoError(t, err)

	require.Len(t, gotClusters, 2)
	assert.ElementsMatch(t, []string{"c-a", "c-b"}, gotClusters)
}

// TestGetTokenClusterDisplayNameResolves is the round-3 repro for finding
// 3: an unresolved display name passed to --cluster must resolve to the
// cluster's id (via the cluster list, using the stored credential) before
// minting — a raw, unresolved display name must never reach spec.clusterName.
func TestGetTokenClusterDisplayNameResolves(t *testing.T) {
	var gotCluster, gotAuth string
	var listHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == clustersPath:
			listHits.Add(1)
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"c-m-abc"},"spec":{"displayName":"prod"}}]}`))
		case r.URL.Path == MintTokensPath && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			gotCluster = gjson.GetBytes(body, "spec.clusterName").String()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"minted"},"status":{"bearerToken":"ext/minted:secret"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prodserver"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prodserver": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "cluster": "prod"}},
		ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "c-m-abc", gotCluster, "the resolved id, not the raw display name, must reach the mint")
	assert.Equal(t, int32(1), listHits.Load())
	assert.Equal(t, "Bearer stored-login-token", gotAuth, "cluster resolution uses the stored credential, not a fresh session")
}

// TestGetTokenClusterMissErrorsWithoutMinting is the round-3 repro for
// finding 3's "unresolvable → propagate the resolution error" half: an
// unresolvable --cluster must fail directly, never mint an unscoped token.
func TestGetTokenClusterMissErrorsWithoutMinting(t *testing.T) {
	var mintHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case clustersPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[]}`))
		case MintTokensPath:
			mintHits.Add(1)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prodserver"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prodserver": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "cluster": "missing"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, int32(0), mintHits.Load(), "an unresolvable cluster must never reach the mint call")
}

// TestGetTokenClusterIDDisplayCollisionIsAmbiguous is the round-4 repro:
// resolveMintClusterID must not let an id-based match short-circuit ahead
// of checking every cluster's spec.displayName too. A string that matches
// one cluster's id AND a different cluster's display name is ambiguous —
// silently minting a token scoped to the id match would produce a token
// for the wrong cluster whenever an admin names a cluster's display name
// after another cluster's raw id.
func TestGetTokenClusterIDDisplayCollisionIsAmbiguous(t *testing.T) {
	var listHits, mintHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case clustersPath:
			atomic.AddInt32(&listHits, 1)
			w.Header().Set("Content-Type", "application/json")
			// "c-1" is both a real cluster's id AND a different cluster's
			// display name. "c-xyz" is a non-collider (distinct id AND
			// display name): it must not appear in the narrowed candidates.
			_, _ = w.Write([]byte(`{"items":[
				{"metadata":{"name":"c-1"},"spec":{"displayName":"prod"}},
				{"metadata":{"name":"c-2"},"spec":{"displayName":"c-1"}},
				{"metadata":{"name":"c-xyz"},"spec":{"displayName":"staging"}}
			]}`))
		case MintTokensPath:
			atomic.AddInt32(&mintHits, 1)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prodserver"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prodserver": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "cluster": "c-1"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.ElementsMatch(t, []string{"c-1", "c-2"}, candidateIDsToken(ce.Candidates))
	assert.NotContains(t, candidateIDsToken(ce.Candidates), "c-xyz",
		"non-collider must be excluded from narrowed candidates")
	assert.Equal(t, int32(1), atomic.LoadInt32(&listHits), "no list scan beyond the collision check")
	assert.Equal(t, int32(0), atomic.LoadInt32(&mintHits), "an ambiguous cluster must never reach the mint call")
}

// TestGetTokenRejectsLegacyPrefixName is the fix-3 failing test: the native
// get-token command must reject --name values starting with "legacy:" (they
// are reserved for the legacy token shim's cache slots). Without fix 3 this
// test fails because no such validation exists.
func TestGetTokenRejectsLegacyPrefixName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := NewTokenCommand(dir, config.NewFileStore(dir)).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "https://r.ex", "name": "legacy:foo"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.ErrorContains(t, ce, "legacy:")
}

// TestGetTokenRejectsLegacyPrefixNameBeforeCacheLookup verifies that the
// reserved-prefix rejection happens BEFORE any cache read: a warm legacy:-slot
// entry must not be served — the VALIDATION error must surface even when the
// cache holds a valid token for that key. Without the run()-level guard this
// test fails because probeFlagCache returns a cache hit before runWithParams
// ever checks IsLegacyCacheSlot.
func TestGetTokenRejectsLegacyPrefixNameBeforeCacheLookup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	serverID := config.CanonicalServerIdentity("https://r.ex")
	key := config.CacheKey("legacy:foo", serverID, "")
	require.NoError(t, store.SetCachedCred(key, "warm-tok", time.Now().Add(time.Hour), "legacy:foo", serverID, ""))

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "https://r.ex", "name": "legacy:foo"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce, "must fail with a VALIDATION error")
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, ce, "legacy:")

	// The cache entry must be untouched — it was never served.
	tok, _, cerr := store.GetCachedCred(key)
	require.NoError(t, cerr, "cache entry must still be present")
	assert.Equal(t, "warm-tok", tok, "cache entry must be untouched")
}

// errCacheStore wraps a CredStore but returns a fixed error from GetCachedCred,
// so fix-4's double-notice test can inject a repeatable store failure without
// corrupting any other store method.
type errCacheStore struct {
	config.CredStore
	cacheErr error
}

func (s *errCacheStore) GetCachedCred(key string) (string, time.Time, error) {
	return "", time.Time{}, s.cacheErr
}

// TestCacheReadErrorNoticedOnce verifies that a single get-token invocation
// with a store that errors on GetCachedCred produces the "cache read failed"
// notice exactly once — even when the resolved cache key differs from the
// fast-path probed key (which happens whenever the current context fills in a
// non-empty cluster). The original fix-4 fixture had no current-context
// cluster, so flagKey == cacheKey; the sub-test below exercises the masked
// gap the re-review identified.
func TestCacheReadErrorNoticedOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 404 everywhere: looks like legacy Rancher (no ext.cattle.io).
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	t.Run("equal flag key and resolved key (no context cluster)", func(t *testing.T) {
		dir := t.TempDir()
		base := config.NewFileStore(dir)
		require.NoError(t, base.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
		cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())

		store := &errCacheStore{CredStore: base, cacheErr: errors.New("disk failure")}
		var notices bytes.Buffer
		cmd := &tokenCmd{dir: dir, store: store}
		_, err := cmd.Execute(t.Context(),
			tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
			ui.New(&notices, strings.NewReader(""), true, false))
		require.NoError(t, err, "a cache read error must not block the full auth path")
		count := strings.Count(notices.String(), "cache read failed")
		assert.Equal(t, 1, count, "cache read error must produce exactly one notice per invocation, not two")
	})
}

// candidateIDsToken extracts candidate ids for an ElementsMatch assertion
// (mirrors the candidateIDs helper already used elsewhere in this package
// for a different type's Candidates field).
func candidateIDsToken(cs []clierr.Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

// TestStoredTokenForServerNoticeScopedToWinningRank is the round-4 repro
// for the cosmetic finding: the multiple-candidates notice must fire only
// when the WINNING rank itself had more than one token-bearing candidate —
// a rank-0 exact --name win must stay silent even if an unrelated,
// lower-ranked server happens to also carry a stale token.
func TestStoredTokenForServerNoticeScopedToWinningRank(t *testing.T) {
	store := config.NewFileStore(t.TempDir())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod-a"}, "tok-a")) // exact --name match, rank 0
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod-z"}, "tok-z")) // unrelated, rank 3 (stale)
	cfg := &config.Config{
		Servers: map[string]config.Server{
			"prod-a": {URL: "https://r.ex"},
			"prod-z": {URL: "https://r.ex"},
		},
	}

	var notices bytes.Buffer
	name, tok, _, ok, serr := storedTokenForServer(cfg, store, "https://r.ex", "prod-a", false, ui.New(&notices, strings.NewReader(""), true, false))
	require.NoError(t, serr)
	require.True(t, ok)
	assert.Equal(t, "prod-a", name)
	assert.Equal(t, "tok-a", tok)
	assert.Empty(t, notices.String(), "a rank-0 exact --name win is a certainty, not a tie-break")
}

// TestGetTokenSurfacesRealMintFailureNotInteractive is the round-2 repro for
// finding 6: a non-auth mint failure (a 500 here, standing in for
// network/TLS failures too) must surface as the real error, never papered
// over as "interactive login required".
func TestGetTokenSurfacesRealMintFailureNotInteractive(t *testing.T) {
	var authProviderHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1-public/authproviders"):
			authProviderHits.Add(1)
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == MintTokensPath && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.Discard())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "interactive login required")
	require.ErrorContains(t, err, "500")
	assert.Equal(t, int32(0), authProviderHits.Load(), "a real mint failure must not fall through to the interactive flow")
}

// TestGetTokenNoticesUnreadableConfigOnStoredCredPath is the round-2 repro
// for finding 7: a config.yaml the stored-credential path can't parse must
// be noticed, not silently folded into a generic "requires login" error.
func TestGetTokenNoticesUnreadableConfigOnStoredCredPath(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("\t: not yaml ["), 0o600))

	var notices bytes.Buffer
	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	// --cluster and a valid (httpx.New parses it into a cert pool) --cacert
	// are set explicitly so resolveConn's OWN config.yaml read is skipped
	// (all three of server/cluster/caIn are non-empty) — the point of this
	// test is storedCredToken's separate, unconditional config.Load call.
	_, err := cmd.Execute(t.Context(),
		tin{
			s: map[string]string{
				"server": "https://r.ex", "cluster": "c-1",
				"cacert": selfSignedCAPEM(t),
				"name":   "prod",
			},
		},
		ui.New(&notices, strings.NewReader(""), true, false))
	require.Error(t, err, "no stored credential and no reachable interactive provider — some error is expected")
	assert.Contains(t, notices.String(), "config unreadable")
}

// TestStoredTokenForServerTieBreak is the round-2 repro for finding 8: two
// Servers entries sharing a BaseURL must resolve deterministically —
// preferring the current context's server, else the lexicographically
// smallest name — not whichever a Go map iteration happens to yield.
func TestStoredTokenForServerTieBreak(t *testing.T) {
	store := config.NewFileStore(t.TempDir())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod-a"}, "tok-a"))
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod-b"}, "tok-b"))
	cfg := &config.Config{
		Servers: map[string]config.Server{
			"prod-a": {URL: "https://r.ex"},
			"prod-b": {URL: "https://r.ex"},
		},
	}

	t.Run("no current context prefers the lexicographically smallest name", func(t *testing.T) {
		name, tok, _, ok, serr := storedTokenForServer(cfg, store, "https://r.ex", "", false, ui.Discard())
		require.NoError(t, serr)
		require.True(t, ok)
		assert.Equal(t, "prod-a", name)
		assert.Equal(t, "tok-a", tok)
	})

	t.Run("the current server wins the tie, regardless of name order", func(t *testing.T) {
		cfg.CurrentServer = "prod-b"
		name, tok, _, ok, serr := storedTokenForServer(cfg, store, "https://r.ex", "", false, ui.Discard())
		require.NoError(t, serr)
		require.True(t, ok)
		assert.Equal(t, "prod-b", name)
		assert.Equal(t, "tok-b", tok)
	})
}

// TestStoredTokenForServerExhaustsCandidatesWithNoToken is the round-3
// repro for finding 1: the tie-break's chosen candidate having no stored
// token must not mask a sibling that does — every candidate is tried, not
// just the top-ranked one.
func TestStoredTokenForServerExhaustsCandidatesWithNoToken(t *testing.T) {
	store := config.NewFileStore(t.TempDir())
	// prod-a ranks first lexicographically but has no stored token.
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod-b"}, "tok-b"))
	cfg := &config.Config{
		Servers: map[string]config.Server{
			"prod-a": {URL: "https://r.ex"},
			"prod-b": {URL: "https://r.ex"},
		},
	}
	name, tok, _, ok, serr := storedTokenForServer(cfg, store, "https://r.ex", "", false, ui.Discard())
	require.NoError(t, serr)
	require.True(t, ok, "a tokenless top-ranked candidate must not short-circuit the search")
	assert.Equal(t, "prod-b", name)
	assert.Equal(t, "tok-b", tok)
}

// TestStoredTokenForServerNamePicksNamedServer is the round-3 repro for
// finding 2: an explicit --name must win over the lexicographic default,
// even when neither is the current context's server.
func TestStoredTokenForServerNamePicksNamedServer(t *testing.T) {
	store := config.NewFileStore(t.TempDir())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod-a"}, "tok-a"))
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod-b"}, "tok-b"))
	cfg := &config.Config{
		Servers: map[string]config.Server{
			"prod-a": {URL: "https://r.ex"},
			"prod-b": {URL: "https://r.ex"},
		},
	}
	name, tok, _, ok, serr := storedTokenForServer(cfg, store, "https://r.ex", "prod-b", false, ui.Discard())
	require.NoError(t, serr)
	require.True(t, ok)
	assert.Equal(t, "prod-b", name, "--name must win over the lexicographic default")
	assert.Equal(t, "tok-b", tok)
}

// TestStoredTokenForServerNoAmbientIgnoresLegacyNamedEntry is the round-7
// finding-3 repro: a config.yaml entry literally named "legacy:<user>" (a
// hand-edited file, or one written before auth login rejected the prefix)
// must NOT win the exact-name tier for a shim call. Under noAmbientContext,
// wantName is a cache-slot label with no entry-name preference, so selection
// follows sorted order alone — "aaa" here, not the exact-name match, which
// sorts after it.
func TestStoredTokenForServerNoAmbientIgnoresLegacyNamedEntry(t *testing.T) {
	store := config.NewFileStore(t.TempDir())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "legacy:zed"}, "tok-legacy"))
	require.NoError(t, store.SetToken(config.ServerRef{Name: "aaa"}, "tok-aaa"))
	cfg := &config.Config{
		Servers: map[string]config.Server{
			"legacy:zed": {URL: "https://r.ex"},
			"aaa":        {URL: "https://r.ex"},
		},
	}
	name, tok, _, ok, serr := storedTokenForServer(cfg, store, "https://r.ex", "legacy:zed", true, ui.Discard())
	require.NoError(t, serr)
	require.True(t, ok)
	assert.Equal(t, "aaa", name, "sorted order must win; wantName carries no preference under noAmbientContext")
	assert.Equal(t, "tok-aaa", tok)
}

// TestStoredTokenForServerMatchesEquivalentURLSpellings is the impl-review
// finding-4 repro: candidate matching must use canonical server identity, so
// a configured "https://host" is found for a "HTTPS://HOST:443" query (case
// and an explicit default port are irrelevant) rather than skipped.
func TestStoredTokenForServerMatchesEquivalentURLSpellings(t *testing.T) {
	store := config.NewFileStore(t.TempDir())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "tok"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://host"}}}

	name, tok, _, ok, serr := storedTokenForServer(cfg, store, "HTTPS://HOST:443", "", false, ui.Discard())
	require.NoError(t, serr)
	require.True(t, ok, "an equivalent URL spelling must still match the configured server")
	assert.Equal(t, "prod", name)
	assert.Equal(t, "tok", tok)
}

func TestSelectAuthProviderExplicitMatch(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&LocalProvider{AuthProvider{Type: "localProvider"}}, &AuthProvider{Type: "pingProvider"}}
	got, err := selectAuthProvider(t.Context(), ps, "pingProvider", ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "pingProvider", got.GetType())
}

func TestSelectAuthProviderNotFound(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&AuthProvider{Type: "pingProvider"}}
	_, err := selectAuthProvider(t.Context(), ps, "nope", ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Contains(t, candidateIDs(ce.Candidates), "pingProvider")
}

func TestSelectAuthProviderSingle(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&AuthProvider{Type: "pingProvider"}}
	got, err := selectAuthProvider(t.Context(), ps, "", ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "pingProvider", got.GetType())
}

func TestSelectAuthProviderAmbiguousNoInput(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&AuthProvider{Type: "pingProvider"}, &AuthProvider{Type: "oktaProvider"}}
	_, err := selectAuthProvider(t.Context(), ps, "", ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.ElementsMatch(t, []string{"pingProvider", "oktaProvider"}, candidateIDs(ce.Candidates))
}

func candidateIDs(cs []clierr.Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

// lineReader yields one line at a time without buffering, so multiple readLine
// calls (each creating a fresh Scanner) don't lose data to Scanner's internal buffer.
type lineReader struct {
	lines []string
	pos   int
}

func newLineReader(lines ...string) *lineReader {
	return &lineReader{lines: lines}
}

func (r *lineReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.lines) {
		return 0, io.EOF
	}
	line := r.lines[r.pos] + "\n"
	r.pos++
	n := copy(p, line)
	return n, nil
}

func TestGetTokenSAMLNonInteractiveIsAuthError(t *testing.T) {
	// A SAML provider under a non-interactive prompter must fast-fail AUTH(3),
	// not open a browser and poll for 15 minutes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"pingProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cmd := NewTokenCommand(dir, config.NewFileStore(dir)).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "c-1"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
}

func TestResolveConnConfigFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	acmeCA := "-----BEGIN CERTIFICATE-----\nMIICertDataAcme\n-----END CERTIFICATE-----\n"
	otherCA := "-----BEGIN CERTIFICATE-----\nMIICertDataOther\n-----END CERTIFICATE-----\n"
	cfg := &config.Config{
		CurrentServer: "acme",
		Servers: map[string]config.Server{
			"acme":  {URL: "https://config.example", CA: acmeCA},
			"other": {URL: "https://other.example", CA: otherCA},
		},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := tokenCmd{dir: dir, store: store}

	t.Run("no server flag sources server from config", func(t *testing.T) {
		server, ca, cluster, err := cmd.resolveConn("", "c-1", "", false)
		require.NoError(t, err)
		assert.Equal(t, "https://config.example", server)
		assert.Equal(t, "c-1", cluster)
		assert.Equal(t, acmeCA, ca, "inline PEM passes through resolveCA")
	})

	t.Run("explicit --server matching another configured entry uses that entry's CA, not the current server's", func(t *testing.T) {
		server, ca, cluster, err := cmd.resolveConn("https://other.example", "c-1", "", false)
		require.NoError(t, err)
		assert.Equal(t, "https://other.example", server)
		assert.Equal(t, "c-1", cluster)
		assert.Equal(t, otherCA, ca, "CA comes from the entry matching the requested server")
	})

	t.Run("explicit --server matching no configured entry gets no custom CA", func(t *testing.T) {
		server, ca, cluster, err := cmd.resolveConn("https://flag.example", "c-1", "", false)
		require.NoError(t, err)
		assert.Equal(t, "https://flag.example", server)
		assert.Equal(t, "c-1", cluster)
		assert.Empty(t, ca, "no configured entry matches — never borrow the current server's CA for a different server")
	})

	t.Run("empty cluster is a validation error on the native path", func(t *testing.T) {
		_, _, _, err := cmd.resolveConn("https://config.example", "", "", false)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.ErrorContains(t, ce, "--cluster is required")
	})

	t.Run("empty cluster on shim path does not error", func(t *testing.T) {
		// noAmbientContext=true (shim path): empty cluster stays unscoped, no error.
		server, _, cluster, err := cmd.resolveConn("https://config.example", "", "", true)
		require.NoError(t, err)
		assert.Equal(t, "https://config.example", server)
		assert.Empty(t, cluster)
	})
}

func TestGetTokenNoNameWritesCachingDisabledNotice(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	var errBuf strings.Builder
	p := ui.New(&errBuf, strings.NewReader(""), false, false)
	cmd := NewTokenCommand(dir, config.NewFileStore(dir)).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": srv.URL, "cluster": "c-1"}}, p)
	require.Error(t, err, "non-interactive local auth should fail")
	assert.Contains(t, errBuf.String(), "caching disabled", "notice must appear in Err")
}

func TestBasicAuth401IsAuthError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"}]}`))
			return
		}
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cmd := NewTokenCommand(dir, config.NewFileStore(dir)).(runtime.ReadCommand)
	var errBuf strings.Builder
	p := ui.New(&errBuf, newLineReader("alice", "secret"), false, true)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "c-1"}},
		p)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAuth, ce.Code, "401 from login POST must map to CodeAuth")
}

func TestBasicAuthExtTokenResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"}]}`))
			return
		}
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"apiVersion": "ext.cattle.io/v1",
				"kind": "Token",
				"metadata": {"name": "token-e2e"},
				"status": {"bearerToken": "ext/token-e2e:sssss", "expiresAt": ""}
			}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cmd := NewTokenCommand(dir, config.NewFileStore(dir)).(runtime.ReadCommand)
	var errBuf strings.Builder
	p := ui.New(&errBuf, newLineReader("alice", "secret"), false, true)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "c-1"}},
		p)
	require.NoError(t, err)

	cred := res.Data.(*execCredential)
	assert.Equal(t, "ext/token-e2e:sssss", cred.Status.Token,
		"ext bearer must reach the exec credential with its prefix intact (EXT-3)")
}

func TestGetTokenCacheHitExtToken(t *testing.T) {
	// An ext-issued bearer ("ext/<name>:<value>") must round-trip the exec-cred
	// cache byte-for-byte — the ext/ prefix is opaque data (EXT-3).
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetCachedCred(config.CacheKey("prod", "https://r.ex", "c-1"), "ext/token-abc:secretxyz", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))
	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)

	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "https://r.ex", "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "ext/token-abc:secretxyz", cred.Status.Token)
}

func TestOAuthDeviceCodeFlowNoInputIsAuthError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"azureADProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cmd := NewTokenCommand(dir, config.NewFileStore(dir)).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "c-1", "auth-flow": "devicecode"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAuth, ce.Code, "device-code flow with NoInput must fast-fail AUTH before network")
}

func TestOAuthDeviceCodeFlowNonTTYAllowed(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"azureADProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	var errBuf strings.Builder
	p := ui.New(&errBuf, strings.NewReader(""), false, false)
	cmd := NewTokenCommand(dir, config.NewFileStore(dir)).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "auth-flow": "devicecode"}},
		p)
	require.Error(t, err, "device-code will fail on OAuth config/network, but not on the gate")
	assert.NotContains(t, err.Error(), "interactive login required", "non-TTY must bypass the gate, fail later")
}

func TestOAuthAuthCodeFlowNoInputIsAuthError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"azureADProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cmd := NewTokenCommand(dir, config.NewFileStore(dir)).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "c-1", "auth-flow": "authcode"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAuth, ce.Code, "authcode flow with NoInput must fast-fail AUTH before network")
}

func TestOAuthAuthCodeFlowNonTTYAllowed(t *testing.T) {
	// Not parallel: sets a process-wide env var. authcode, once past the gate,
	// opens a browser and waits ~5m on a loopback callback; forcing an invalid
	// callback port makes it fail fast right after the gate, so the test proves
	// the gate is bypassed for non-TTY without entering that wait.
	t.Setenv("RANCHER_CLI_OAUTH_CALLBACK_PORT", "70000") // out of range -> getCallbackPort errors
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"azureADProvider","authUrl":"https://idp.example/authorize","deviceAuthUrl":"https://idp.example/devicecode","tokenUrl":"https://idp.example/token"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	var errBuf strings.Builder
	p := ui.New(&errBuf, strings.NewReader(""), false, false)
	cmd := NewTokenCommand(dir, config.NewFileStore(dir)).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "c-1", "auth-flow": "authcode"}},
		p)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "interactive login required", "non-TTY must bypass the gate")
	assert.ErrorContains(t, err, "callback port", "should fail past the gate on the forced port error")
}

func TestOAuthAuthCodeAuthHonorsCallerContext(t *testing.T) {
	// Regression pin for the exchange-ctx/rancherLogin-ctx shadowing bug: the
	// caller's context must still govern the flow after the code exchange,
	// with zero timing sensitivity (no slow-IdP race).
	t.Parallel()

	var rancherHits atomic.Int32
	rancherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rancherHits.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer rancherSrv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// openBrowser stub: drives the local callback synchronously (queuing the
	// buffered resultCh before the select ever runs), then cancels the caller
	// ctx. Ordering is deterministic — no goroutine race, no timing window.
	openBrowserStub := func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		cbURL := q.Get("redirect_uri") + "?code=fake-code&state=" + url.QueryEscape(q.Get("state"))
		resp, err := http.Get(cbURL)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		cancel()
		return nil
	}

	provider := &AzureADProvider{
		OAuthProvider: OAuthProvider{
			ClientID: "test-client",
			Scopes:   []string{"openid"},
			OAuthEndpoint: OAuthEndpoint{
				AuthURL:       "https://idp.example/authorize",
				DeviceAuthURL: "https://idp.example/devicecode",
				TokenURL:      "https://idp.example/token",
			},
		},
	}
	input := &loginInput{server: rancherSrv.URL, authProvider: "azureADProvider", idpClient: http.DefaultClient}

	_, err := oauthAuthCodeAuth(ctx, http.DefaultClient, input, provider, 5*time.Second, openBrowserStub, ui.Discard())
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled, "error must wrap context.Canceled, got: %v", err)
	assert.Equal(t, int32(0), rancherHits.Load(),
		"Rancher login must never be reached once the caller context is cancelled")
}

func TestOAuthAuthCodeAuthAbortsOnCallerCancelDuringBrowserWait(t *testing.T) {
	// Regression pin: cancellation while still WAITING for the browser
	// callback (no callback ever arrives) must abort promptly via the wait
	// select's ctx.Done() case, not surface later as a confusing exchange
	// failure or wait for the flow timeout.
	t.Parallel()

	var rancherHits atomic.Int32
	rancherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rancherHits.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer rancherSrv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// openBrowser stub: never drives the callback — cancels the caller ctx
	// immediately instead, so by the time the wait select runs, ctx.Done() is
	// already the only ready case (deterministic, no timing window).
	openBrowserStub := func(authURL string) error {
		cancel()
		return nil
	}

	provider := &AzureADProvider{
		OAuthProvider: OAuthProvider{
			ClientID: "test-client",
			Scopes:   []string{"openid"},
			OAuthEndpoint: OAuthEndpoint{
				AuthURL:       "https://idp.example/authorize",  // never reached
				DeviceAuthURL: "https://idp.example/devicecode", // never reached
				TokenURL:      "https://idp.example/token",
			},
		},
	}
	input := &loginInput{server: rancherSrv.URL, authProvider: "azureADProvider", idpClient: http.DefaultClient}

	_, err := oauthAuthCodeAuth(ctx, http.DefaultClient, input, provider, 5*time.Second, openBrowserStub, ui.Discard())
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled, "error must wrap context.Canceled, got: %v", err)
	assert.Equal(t, int32(0), rancherHits.Load())
}

// DR-009 repro: the loopback callback used to reflect error/error_description
// query params into its HTML response unescaped. A malicious redirect to the
// callback URL (or a compromised IdP) must not get its markup executed.
func TestOAuthCallbackEscapesReflectedErrorParams(t *testing.T) {
	t.Parallel()

	var body []byte
	openBrowserStub := func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		cbURL := q.Get("redirect_uri") + "?state=" + url.QueryEscape(q.Get("state")) +
			"&error=" + url.QueryEscape("bad<script>alert(1)</script>") +
			"&error_description=" + url.QueryEscape("<img src=x onerror=alert(2)>")
		resp, err := http.Get(cbURL)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err = io.ReadAll(resp.Body)
		return err
	}

	provider := &AzureADProvider{
		OAuthProvider: OAuthProvider{
			ClientID: "test-client",
			Scopes:   []string{"openid"},
			OAuthEndpoint: OAuthEndpoint{
				AuthURL:       "https://idp.example/authorize",  // never reached
				DeviceAuthURL: "https://idp.example/devicecode", // never reached
				TokenURL:      "https://idp.example/token",
			},
		},
	}
	input := &loginInput{server: "http://ignored", authProvider: "azureADProvider", idpClient: http.DefaultClient}

	_, err := oauthAuthCodeAuth(t.Context(), http.DefaultClient, input, provider, 5*time.Second, openBrowserStub, ui.Discard())
	require.Error(t, err)
	assert.NotContains(t, string(body), "<script>", "reflected markup must be escaped, got %q", body)
	assert.NotContains(t, string(body), "<img", "reflected markup must be escaped, got %q", body)
	assert.Contains(t, string(body), "&lt;script&gt;")
}

// DR-009 repro: an unsolicited request with the wrong (or no) state must not
// consume the callback's one-shot handler — otherwise the real IdP redirect
// that arrives afterward gets no response at all.
func TestOAuthCallbackWrongStateWithErrorDoesNotConsumeHandler(t *testing.T) {
	t.Parallel()

	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fake-access-token","token_type":"bearer","expires_in":3600}`))
	}))
	defer idp.Close()

	rancherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer rancherSrv.Close()

	openBrowserStub := func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		redirect := q.Get("redirect_uri")

		attackURL := redirect + "?state=wrong-state&error=access_denied&error_description=nope"
		attackResp, err := http.Get(attackURL)
		if err != nil {
			return err
		}
		_ = attackResp.Body.Close()
		if attackResp.StatusCode != http.StatusBadRequest {
			return errors.New("forged wrong-state request did not get 400 Bad Request")
		}

		cbURL := redirect + "?code=fake-code&state=" + url.QueryEscape(q.Get("state"))
		resp, err := http.Get(cbURL)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}

	provider := &AzureADProvider{
		OAuthProvider: OAuthProvider{
			ClientID: "test-client",
			Scopes:   []string{"openid"},
			OAuthEndpoint: OAuthEndpoint{
				AuthURL:       idp.URL + "/authorize",
				DeviceAuthURL: idp.URL + "/devicecode",
				TokenURL:      idp.URL + "/token",
			},
		},
	}
	input := &loginInput{server: rancherSrv.URL, authProvider: "azureADProvider", idpClient: idp.Client()}

	var noticeBuf strings.Builder
	p := ui.New(&noticeBuf, strings.NewReader(""), true, false)

	tok, err := oauthAuthCodeAuth(t.Context(), http.DefaultClient, input, provider, 5*time.Second, openBrowserStub, p)
	require.NoError(t, err, "the real redirect must still be processed after a forged wrong-state request")
	require.NotNil(t, tok)
	assert.Contains(t, noticeBuf.String(), "invalid state",
		"a forged-only callback must not hang silently — the user must see why the wait continues")
}

// DR-010 repro: samlAuth's poll select had no ctx.Done() case, unlike the
// OAuth authcode flow's equivalent select — SIGTERM/caller cancellation was
// only honored on the next poll tick instead of promptly. Shrinking the poll
// interval and cancelling well before the first tick proves the fix: without
// the ctx.Done() case, samlAuth would not return until that first tick fired.
func TestSAMLAuthAbortsPromptlyOnCtxCancel(t *testing.T) {
	origTimeout, origPoll := samlAuthTimeout, samlPollInterval
	samlAuthTimeout = time.Minute
	samlPollInterval = 500 * time.Millisecond
	defer func() { samlAuthTimeout, samlPollInterval = origTimeout, origPoll }()

	// The server lives outside the synctest bubble and is never contacted:
	// cancellation wins the select before the first poll tick fires, so the
	// bubble never blocks on real network I/O.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // token never ready — keeps polling
	}))
	defer srv.Close()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		input := &loginInput{server: srv.URL}

		go func() {
			time.Sleep(50 * time.Millisecond) // fake clock — before the first 500ms tick
			cancel()
		}()

		start := time.Now()
		_, err := samlAuth(ctx, http.DefaultClient, input, ui.Discard())
		elapsed := time.Since(start)

		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled, "error must wrap context.Canceled, got: %v", err)
		// Fake time: RSA keygen and scheduling cost zero, so elapsed is the
		// cancel goroutine's 50ms exactly — anything at or past the tick
		// means the select waited for the poll instead of the cancellation.
		assert.Less(t, elapsed, samlPollInterval,
			"must abort on ctx cancellation, not wait for the next poll tick")
	})
}

// blockingReader never returns from Read, so readLine's scan goroutine cannot
// win the select against a cancelled context — making the ctrl-C test deterministic.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }

func TestSelectAuthProviderCtrlCIsNotAmbiguous(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var errBuf strings.Builder
	// A blocking reader (not an empty reader): an empty reader hits EOF instantly,
	// racing ctx.Done() in the select and intermittently returning io.EOF.
	p := ui.New(&errBuf, blockingReader{}, false, true)
	providers := []typedProvider{&AuthProvider{Type: "pingProvider"}, &AuthProvider{Type: "oktaProvider"}}
	_, err := selectAuthProvider(ctx, providers, "", p)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled, "cancelled context must return context.Canceled, not AMBIGUOUS_SELECTION")
}

func TestGetAuthProviders404IsError(t *testing.T) {
	t.Parallel()
	// Record every request path: the assertion below must FAIL if the old
	// 404→/v3-public fallback somehow survives, not just when the error text
	// changes (a surviving fallback would also 404 and produce the same error).
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cmd := NewTokenCommand(dir, config.NewFileStore(dir)).(runtime.ReadCommand)
	var errBuf strings.Builder
	p := ui.New(&errBuf, newLineReader(), false, true)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "c-1"}},
		p)
	require.Error(t, err)
	require.ErrorContains(t, err, "404", "a 404 from /v1-public/authproviders must surface as an error")
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, paths)
	for _, p := range paths {
		assert.NotContains(t, p, "v3-public", "no request may fall back to the removed /v3-public API")
	}
}

// TestApplyDefaultSchemeHostPort verifies that a host:port authority without
// an explicit scheme receives the https:// default.
func TestApplyDefaultSchemeHostPort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"host:8443", "https://host:8443"},
		{"[::1]", "https://[::1]"},
		{"[::1]:8443", "https://[::1]:8443"},
		{"host", "https://host"},
		{"http://host", "http://host"},
		{"https://host:8443", "https://host:8443"},
		{"ftp://host", "ftp://host"},
		{"", ""},
	}
	for _, tc := range cases {
		got := applyDefaultScheme(tc.in)
		assert.Equal(t, tc.want, got, "applyDefaultScheme(%q)", tc.in)
	}
}

// TestGetTokenHostPortWarmCacheHit verifies that a --server host:port flag
// (schemeless) normalizes to https://host:port and hits a warm cache entry.
func TestGetTokenHostPortWarmCacheHit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	key := config.CacheKey("prod", config.CanonicalServerIdentity("https://host:8443"), "c-1")
	require.NoError(t, store.SetCachedCred(key, "tok-8443", time.Now().Add(time.Hour), "prod", "https://host:8443", "c-1"))
	// Break config — if fast path misses the full path fails, proving no hit.
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("\t: not yaml ["), 0o600))

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "host:8443", "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "tok-8443", cred.Status.Token, "host:port flag must hit the https:// warm cache record")
}

// TestGetTokenIPv6WarmCacheHit verifies that a --server [::1]:8443 flag
// (schemeless bracketed IPv6) normalizes to https://[::1]:8443 and hits a
// warm cache entry.
func TestGetTokenIPv6WarmCacheHit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	key := config.CacheKey("prod", config.CanonicalServerIdentity("https://[::1]:8443"), "c-1")
	require.NoError(t, store.SetCachedCred(key, "tok-ipv6", time.Now().Add(time.Hour), "prod", "https://[::1]:8443", "c-1"))
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("\t: not yaml ["), 0o600))

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "[::1]:8443", "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "tok-ipv6", cred.Status.Token, "[::1]:port flag must hit the https:// warm cache record")
}

// TestGetTokenMalformedServerNoFastPathCacheHit verifies that a malformed
// --server value (e.g. https://[::1 — unclosed bracket) cannot produce a
// fast-path cache hit even when a matching cache record exists under the
// lowercased fallback identity.
func TestGetTokenMalformedServerNoFastPathCacheHit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	// Seed a record under the lowercased-fallback identity that
	// CanonicalServerIdentity produces for the malformed URL.
	malformed := "https://[::1"
	fallbackID := config.CanonicalServerIdentity(malformed)
	key := config.CacheKey("prod", fallbackID, "c-1")
	require.NoError(t, store.SetCachedCred(key, "should-not-be-served", time.Now().Add(time.Hour), "prod", fallbackID, "c-1"))
	// Break config — if fast path incorrectly hits, no error is returned.
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("\t: not yaml ["), 0o600))

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": malformed, "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	require.Error(t, err, "malformed --server must not produce a fast-path cache hit")
}

// TestGetTokenUnsupportedSchemeNoFastPathCacheHit verifies that a --server
// value with an unsupported scheme (e.g. ftp://host) cannot produce a
// fast-path cache hit even when a matching cache record exists.
func TestGetTokenUnsupportedSchemeNoFastPathCacheHit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	ftpID := config.CanonicalServerIdentity("ftp://host")
	key := config.CacheKey("prod", ftpID, "c-1")
	require.NoError(t, store.SetCachedCred(key, "should-not-be-served", time.Now().Add(time.Hour), "prod", ftpID, "c-1"))
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("\t: not yaml ["), 0o600))

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "ftp://host", "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	require.Error(t, err, "unsupported scheme --server must not produce a fast-path cache hit")
}

// TestGetTokenHostPortColdPathNormalizesToHTTPS verifies the cold path (empty
// cache, valid minimal config) for a schemeless host:port --server flag: the
// value must be normalised to https://host:port BEFORE the connection attempt,
// so the request actually reaches the HTTPS server at that address.
//
// Without normalisation the schemeless URL reaches getAuthProviders, which
// passes it to http.NewRequestWithContext; the Go HTTP client rejects a
// schemeless URL with "unsupported protocol scheme", the TLS server is never
// contacted, and the hits assertion fails (RED). With normalisation the TLS
// connection succeeds (--cacert pins the server's own certificate) and the
// server records the hit (GREEN).
func TestGetTokenHostPortColdPathNormalizesToHTTPS(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			hits.Add(1)
			// Return a provider so the code advances past provider discovery.
			// The interactive prompter (ui.Discard, NoInput=true) then fails
			// with CodeAuth — the point is the server was reached.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	// addr is "127.0.0.1:PORT" — the schemeless form of the TLS server's address.
	addr := srv.Listener.Addr().String()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	// Minimal valid config: readable, no current context, no stored credential.
	cfg := &config.Config{}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	// Empty cache → cold path. --cacert pins the test server's certificate so
	// the connection attempt can actually reach the test TLS server.
	_, err := cmd.Execute(t.Context(),
		tin{
			s: map[string]string{"server": addr, "name": "prod", "cluster": "c-1", "cacert": serverCAPEM(srv)},
		},
		ui.Discard())
	// Non-interactive localProvider → CodeAuth; the important signal is that
	// the test server was hit, proving https://addr reached the connection path.
	require.Error(t, err)
	assert.Greater(t, hits.Load(), int32(0),
		"normalised https://host:port must reach the connection path, not error on a schemeless URL")
}

// TestGetTokenIPv6ColdPathNormalizesToHTTPS verifies the cold path (empty
// cache, valid minimal config) for a schemeless bracketed IPv6 --server flag:
// [::1]:PORT must normalise to https://[::1]:PORT before the connection attempt,
// so the request actually reaches the HTTPS server at that address.
//
// The test binds a TLS server to [::1]:0 (OS-assigned free port) and skips if
// the IPv6 loopback is unavailable on the host. Without normalisation the
// schemeless URL reaches getAuthProviders, which passes it to
// http.NewRequestWithContext; the Go HTTP client rejects it with "first path
// segment in URL cannot contain colon", the TLS server is never contacted, and
// the hits assertion fails (RED). With normalisation the TLS dial succeeds
// (--cacert pins the server's own certificate) and the server records the hit
// (GREEN).
func TestGetTokenIPv6ColdPathNormalizesToHTTPS(t *testing.T) {
	t.Parallel()

	ln, lerr := net.Listen("tcp6", "[::1]:0")
	if lerr != nil {
		t.Skip("IPv6 loopback not available on this host:", lerr)
	}

	var hits atomic.Int32
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = ln
	srv.StartTLS()
	defer srv.Close()

	// addr is "[::1]:PORT" — the schemeless form of the TLS server's address.
	addr := srv.Listener.Addr().String()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{
			s: map[string]string{"server": addr, "name": "prod", "cluster": "c-1", "cacert": serverCAPEM(srv)},
		},
		ui.Discard())
	require.Error(t, err)
	assert.Greater(t, hits.Load(), int32(0),
		"normalised https://[::1]:port must reach the connection path, not error on a schemeless IPv6 URL")
}

// TestGetTokenFTPServerReadableConfigRejectsResolvedProbe is the fifth-re-review
// repro for the post-resolution probe: a --server ftp://host flag bypasses the
// fast-path validation (which already rejects unsupported schemes), so with a
// readable config the resolved probe must catch it. The test uses a readable
// (empty) config so resolveConn succeeds and the second validation point is
// exercised, proving the seeded cache record is never served.
func TestGetTokenFTPServerReadableConfigRejectsResolvedProbe(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	ftpID := config.CanonicalServerIdentity("ftp://host")
	key := config.CacheKey("prod", ftpID, "c-1")
	require.NoError(t, store.SetCachedCred(key, "should-not-be-served", time.Now().Add(time.Hour), "prod", ftpID, "c-1"))
	// Readable (empty) config: resolveConn succeeds, proving the resolved probe is reached.

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "ftp://host", "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	require.Error(t, err, "ftp:// server must be rejected at the resolved-probe validation, not served from cache")
	assert.ErrorContains(t, err, "ftp://host", "error must name the invalid resolved server")
}

// TestGetTokenMalformedServerReadableConfigRejectsResolvedProbe is the fifth-re-review
// repro for the post-resolution probe: a malformed --server https://[::1 (unclosed
// bracket) bypasses the fast-path (CanonicalServerURL errors → fast path skipped),
// and with a readable config the resolved probe must catch it. The seeded cache
// record under the lowercased-fallback identity must not be served.
func TestGetTokenMalformedServerReadableConfigRejectsResolvedProbe(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	malformed := "https://[::1"
	fallbackID := config.CanonicalServerIdentity(malformed)
	key := config.CacheKey("prod", fallbackID, "c-1")
	require.NoError(t, store.SetCachedCred(key, "should-not-be-served", time.Now().Add(time.Hour), "prod", fallbackID, "c-1"))
	// Readable (empty) config: resolveConn succeeds, proving the resolved probe is reached.

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": malformed, "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	require.Error(t, err, "malformed --server must be rejected at the resolved-probe validation, not served from cache")
	assert.ErrorContains(t, err, malformed, "error must name the invalid resolved server")
}

// TestGetTokenEmptyHostnameServerIsRejected verifies that a --server value with
// an empty hostname (https://:443) cannot address a cache entry: the canonicalizer
// rejects it at the resolved-probe validation and get-token returns an error
// rather than serving the seeded record.
func TestGetTokenEmptyHostnameServerIsRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	emptyHost := "https://:443"
	id := config.CanonicalServerIdentity(emptyHost)
	key := config.CacheKey("prod", id, "c-1")
	require.NoError(t, store.SetCachedCred(key, "should-not-be-served", time.Now().Add(time.Hour), "prod", id, "c-1"))
	// Readable (empty) config.

	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": emptyHost, "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	require.Error(t, err, "https://:443 must not address a cache entry")
	assert.ErrorContains(t, err, emptyHost, "error must name the invalid resolved server")
}

// TestBasicAuthUsernameDefault verifies that a non-empty UsernameDefault on
// runParams (threaded through loginInput) causes the username prompt to show
// the default in brackets, and that an empty answer resolves to that default
// in the login POST body.
func TestBasicAuthUsernameDefault(t *testing.T) {
	t.Parallel()
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"test-tok","token":"test-tok:s"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	var errBuf strings.Builder
	// Empty username answer → the default "alice" must be substituted.
	p := ui.New(&errBuf, newLineReader("", "secret"), false, true)

	input := &loginInput{
		server:          srv.URL,
		authProvider:    "localProvider",
		usernameDefault: "alice",
	}
	tok, err := basicAuth(t.Context(), http.DefaultClient, input, p)
	require.NoError(t, err)

	assert.Contains(t, errBuf.String(), "[alice]", "prompt label must include the default in brackets")
	assert.Equal(t, "alice", gjson.GetBytes(gotBody, "username").String(), "empty answer must fall back to the default")
	assert.Equal(t, "test-tok:s", tok.Bearer)
}

// TestBasicAuthTrimsPaddedUsername is the round-7 finding-4 repro: a
// non-blank answer with incidental padding (a paste with a trailing newline,
// an autocomplete artifact) must be trimmed before it is sent — previously
// TrimSpace was used only for the emptiness check and the raw " bob " went
// into the login POST body.
func TestBasicAuthTrimsPaddedUsername(t *testing.T) {
	t.Parallel()
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"test-tok","token":"test-tok:s"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	var errBuf strings.Builder
	p := ui.New(&errBuf, newLineReader("  bob  ", "secret"), false, true)

	input := &loginInput{server: srv.URL, authProvider: "localProvider"}
	_, err := basicAuth(t.Context(), http.DefaultClient, input, p)
	require.NoError(t, err)
	assert.Equal(t, "bob", gjson.GetBytes(gotBody, "username").String(), "padding must be trimmed from the sent username")
}

// TestCaForServerDeterministicWithMultipleMatches is the round-3 repro for
// finding 2: when multiple cfg.Servers entries share a host, caForServer must
// return the same CA on every call regardless of Go's non-deterministic map
// iteration order. Current-context server wins; otherwise the
// lexicographically first entry name wins.
func TestCaForServerDeterministicWithMultipleMatches(t *testing.T) {
	t.Parallel()
	caAlpha := "-----BEGIN CERTIFICATE-----\nMIICalpha\n-----END CERTIFICATE-----\n"
	caZeta := "-----BEGIN CERTIFICATE-----\nMIICzeta\n-----END CERTIFICATE-----\n"

	t.Run("sorted name wins when no current context", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{
			Servers: map[string]config.Server{
				"alpha": {URL: "https://shared.example", CA: caAlpha},
				"zeta":  {URL: "https://shared.example", CA: caZeta},
			},
		}
		// Run many times: map iteration order varies, so nondeterminism surfaces.
		for i := range 20 {
			got := caForServer(cfg, "https://shared.example", false)
			assert.Equal(t, caAlpha, got, "alpha < zeta: caAlpha must win on every call (iteration %d)", i)
		}
	})

	t.Run("current server wins over sorted order", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{
			CurrentServer: "zeta",
			Servers: map[string]config.Server{
				"alpha": {URL: "https://shared.example", CA: caAlpha},
				"zeta":  {URL: "https://shared.example", CA: caZeta},
			},
		}
		got := caForServer(cfg, "https://shared.example", false)
		assert.Equal(t, caZeta, got, "current server zeta must win over sorted alpha")
	})

	t.Run("noAmbientContext=true ignores current-server preference", func(t *testing.T) {
		t.Parallel()
		// Same config as "current server wins" but with noAmbientContext=true:
		// even though the current server is "zeta", sorted order (alpha) must win.
		cfg := &config.Config{
			CurrentServer: "zeta",
			Servers: map[string]config.Server{
				"alpha": {URL: "https://shared.example", CA: caAlpha},
				"zeta":  {URL: "https://shared.example", CA: caZeta},
			},
		}
		got := caForServer(cfg, "https://shared.example", true)
		assert.Equal(t, caAlpha, got, "noAmbientContext=true: current server zeta must be ignored; sorted alpha wins")
	})
}

// TestNeedsEffGateEachDisjunct is the structural pin for the needsEff gate in
// resolveConn. For each combination of inputs where ALL three disjuncts are
// false, cfg.Effective (and its GetToken call) must not fire. errGetTokenStore
// errors on GetToken for "ambient" — if Effective is called unexpectedly,
// resolveConn returns an error and the test fails.
//
//   - Disjunct 1: server == ""
//   - Disjunct 2: cluster == "" && !noAmbientContext
//   - Disjunct 3: caIn == "" && !explicitServer
//
// All three negations hold in each case below, so needsEff=false.
func TestNeedsEffGateEachDisjunct(t *testing.T) {
	const inlinePEM = "-----BEGIN CERTIFICATE-----\ndGVzdA==\n-----END CERTIFICATE-----"

	cases := []struct {
		name             string
		server           string
		cluster          string
		caIn             string
		noAmbientContext bool
	}{
		{
			// All three flags explicit, noAmbientContext=false:
			// server!="" (1 false), cluster!="" (2 false), caIn!="" (3 false).
			name:   "all explicit flags noAmbientContext false",
			server: "https://test.example.com", cluster: "c-1", caIn: inlinePEM,
			noAmbientContext: false,
		},
		{
			// All three flags explicit, noAmbientContext=true:
			// same logic; noAmbientContext irrelevant when cluster is set.
			name:   "all explicit flags noAmbientContext true",
			server: "https://test.example.com", cluster: "c-1", caIn: inlinePEM,
			noAmbientContext: true,
		},
		{
			// Shim's common case: explicit server + noAmbientContext=true, caIn="".
			// Disjunct 1: server!="" → false.
			// Disjunct 2: cluster="" but noAmbientContext=true → false.
			// Disjunct 3: caIn="" but explicitServer=true → false.
			// Config is loaded (outer if: caIn==""), but Effective must not run.
			name:   "noAmbientContext true empty cluster and caIn",
			server: "https://test.example.com", cluster: "", caIn: "",
			noAmbientContext: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Write a config with a current server pointing at "ambient".
			// If Effective is called, GetToken for "ambient" errors → resolveConn
			// propagates the error and the test catches it.
			cfg := &config.Config{
				CurrentServer: "ambient",
				Servers:       map[string]config.Server{"ambient": {URL: "https://ambient.example.com"}},
			}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())

			base := config.NewFileStore(dir)
			store := &errGetTokenStore{CredStore: base, errName: "ambient"}
			cmd := tokenCmd{dir: dir, store: store}

			_, _, _, err := cmd.resolveConn(tc.server, tc.cluster, tc.caIn, tc.noAmbientContext)
			require.NoError(t, err,
				"resolveConn must not call cfg.Effective when needsEff=false (case %q)", tc.name)
		})
	}
}

// TestResolveConnNoAmbientRequiresServer pins the resolveConn side of the
// blank---server class: a noAmbientContext caller with no server must get the
// "--server is required" validation error without cfg.Effective ever running —
// never the ambient current-context URL. errGetTokenStore errors on GetToken
// for "ambient", so an unexpected Effective call would surface as a different
// error and fail the assertions below.
func TestResolveConnNoAmbientRequiresServer(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "ambient",
		Servers:       map[string]config.Server{"ambient": {URL: "https://ambient.example.com"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	base := config.NewFileStore(dir)
	store := &errGetTokenStore{CredStore: base, errName: "ambient"}
	cmd := tokenCmd{dir: dir, store: store}

	_, _, _, err := cmd.resolveConn("", "", "", true)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.ErrorContains(t, ce, "--server is required")
}

// errDeleteCacheStore wraps a CredStore but fails DeleteCachedCred with a
// fixed error, for pinning delete-token's error classification pass-through.
type errDeleteCacheStore struct {
	config.CredStore
	delErr error
}

func (s *errDeleteCacheStore) DeleteCachedCred(string) error { return s.delErr }

// TestDeleteTokenNamedPreservesLockTimeout pins the exercise-8/9-fixes
// review finding: a named delete whose store delete times out on the
// token-cache lock must surface LOCK_TIMEOUT (exit 8, retryable), matching
// the --all path's pass-through, not the GENERIC catch-all.
func TestDeleteTokenNamedPreservesLockTimeout(t *testing.T) {
	dir := t.TempDir()
	base := config.NewFileStore(dir)
	require.NoError(t, base.SetCachedCred("acme/u/c", "tok", time.Now().Add(time.Hour), "", "", ""))
	store := &errDeleteCacheStore{CredStore: base, delErr: clierr.Retryable(clierr.CodeLockTimeout,
		"locking token-cache.yaml (another rancher process may be writing): context deadline exceeded")}
	cmd := NewDeleteTokenCommand(dir, store).(runtime.ReadCommand)

	_, err := cmd.Execute(t.Context(), tin{a: []string{"acme/u/c"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeLockTimeout, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Equal(t, 8, ce.ExitCode())
	assert.Contains(t, ce.Message, `failed to delete "acme/u/c"`)
	assert.Contains(t, ce.Message, "already deleted")
}
