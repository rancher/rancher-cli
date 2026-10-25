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
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tin struct {
	s map[string]string
	b map[string]bool
	a []string
}

func (t tin) String(k string) string  { return t.s[k] }
func (t tin) Bool(k string) bool      { return t.b[k] }
func (t tin) Strings(string) []string { return nil }
func (t tin) IsSet(k string) bool {
	_, inS := t.s[k]
	_, inB := t.b[k]
	return inS || inB
}

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
		require.NoError(t, seedCache(t, dir, store, "acme/u/c", "tok", time.Now().Add(time.Hour), "", "", ""))
		res, err := cmd.Execute(t.Context(), tin{b: map[string]bool{"all": true}}, ui.Discard())
		require.NoError(t, err)
		assert.Equal(t, "deleted 1 cached credentials\ntokens with no server recorded:\n  not revoked cached token with no name recorded (unproven)\n", res.Text)
		_, _, gerr := store.GetCachedCred("acme/u/c", anchorFor(dir))
		assert.ErrorIs(t, gerr, config.ErrNoToken)
	})

	t.Run("named delete removes the entry", func(t *testing.T) {
		require.NoError(t, seedCache(t, dir, store, "acme/u/c", "tok", time.Now().Add(time.Hour), "", "", ""))
		res, err := cmd.Execute(t.Context(), tin{a: []string{"acme/u/c"}}, ui.Discard())
		require.NoError(t, err)
		assert.Equal(t, "cached credential acme/u/c deleted\ntokens with no server recorded:\n  not revoked cached token with no name recorded (unproven)\n", res.Text)
		_, _, gerr := store.GetCachedCred("acme/u/c", anchorFor(dir))
		assert.ErrorIs(t, gerr, config.ErrNoToken)
	})

	t.Run("--all with a name is a validation error, not a silent flush", func(t *testing.T) {
		require.NoError(t, seedCache(t, dir, store, "keep/u/c", "tok", time.Now().Add(time.Hour), "", "", ""))
		_, err := cmd.Execute(t.Context(), tin{b: map[string]bool{"all": true}, a: []string{"keep/u/c"}}, ui.Discard())
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		// the cache must be untouched - nothing deleted
		_, _, gerr := store.GetCachedCred("keep/u/c", anchorFor(dir))
		require.NoError(t, gerr)
	})

	t.Run("bare name deletes every entry sharing that name", func(t *testing.T) {
		scoped := config.CacheKey("prod", "r.ex", "c-1")
		require.NoError(t, seedCache(t, dir, store, "prod", "legacy", time.Now().Add(time.Hour), "", "", ""))
		require.NoError(t, seedCache(t, dir, store, scoped, "scoped", time.Now().Add(time.Hour), "prod", "r.ex", "c-1"))
		res, err := cmd.Execute(t.Context(), tin{a: []string{"prod"}}, ui.Discard())
		require.NoError(t, err)
		deleted := res.Data.(map[string]any)["deleted"].([]string)
		assert.ElementsMatch(t, []string{"prod", scoped}, deleted)
		_, _, gerr := store.GetCachedCred("prod", anchorFor(dir))
		require.ErrorIs(t, gerr, config.ErrNoToken)
		_, _, gerr = store.GetCachedCred(scoped, anchorFor(dir))
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
	cmd := newTokenCommandForTest(t, dir, config.NewFileStore(dir)).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(), tin{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestGetTokenNoClusterRejectsBeforeCacheProbe(t *testing.T) {
	// A still-valid cache entry keyed with an empty cluster component (written
	// before --cluster became required) must NOT be served: the native command
	// rejects a missing --cluster before any cache read - a token must never
	// be returned for a cluster the caller didn't name.
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, seedCache(t, dir, store, config.CacheKey("prod", "https://r.ex", ""), "cached-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", ""))
	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)

	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "https://r.ex", "name": "prod"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "--cluster is required", ce.Message)
}

// TestGetTokenBrokenConfigMakesEveryCacheHitAMiss asserts the rule that
// replaced the fast path's old promise. A warm record used to be served
// through an unreadable config.yaml, on the reasoning that flags fully
// specified the connection. They do not specify who the credential
// belongs to: that is the entry and its cache binding id, both of which
// live in the file. After a re-login as another user behind a broken
// config, the record served under the old promise is the previous user's
// cluster credential. So the hit fails closed, and the operator sees the
// config error rather than somebody else's token.
func TestGetTokenBrokenConfigMakesEveryCacheHitAMiss(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	key := config.CacheKey("prod", "https://r.ex", "c-1")
	require.NoError(t, seedCache(t, dir, store, key, "cached-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))

	// The anchor is built the way the root hook builds it: a config it
	// cannot parse leaves it with nothing to judge a record against.
	anchor := config.NewCacheAnchor(dir)
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("\t: not yaml ["), 0o600))
	_, lerr := config.Load(config.FilePath(dir))
	require.Error(t, lerr, "the config is unreadable, so the root hook stores no snapshot")

	cmd := tokenCmd{dir: dir, store: store, anchor: anchor}
	_, _, _, hit := cmd.probeFlagCache("prod", "https://r.ex", "c-1", "", "", ui.Discard())
	assert.False(t, hit, "a record whose binding cannot be checked is never served")

	// And the record is left alone: a reader that cannot judge one must
	// not delete a cache that may be perfectly good. With the file
	// readable again the same record serves, so the miss above was the
	// config error and nothing else.
	require.NoError(t, os.Remove(config.FilePath(dir)))
	tok, _, gerr := store.GetCachedCred(key, anchorFor(dir))
	require.NoError(t, gerr)
	assert.Equal(t, "cached-tok", tok, "the record survives a reader that could not judge it")
}

func TestGetTokenCacheKeyScopedByServer(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	// Same --name cached against two servers must not collide.
	require.NoError(t, seedCache(t, dir, store, config.CacheKey("prod", "https://r1.example.com", "c-1"), "tok-r1", time.Now().Add(time.Hour), "prod", "https://r1.example.com", "c-1"))
	require.NoError(t, seedCache(t, dir, store, config.CacheKey("prod", "https://r2.example.com", "c-1"), "tok-r2", time.Now().Add(time.Hour), "prod", "https://r2.example.com", "c-1"))
	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)

	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "https://r2.example.com", "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "tok-r2", cred.Status.Token, "get-token must return the token scoped to the requested server")
}

// TestGetTokenSchemelessFlagSkipsLegacyHostOnlyKey asserts that
// --server host (schemeless) must NOT serve a legacy host-only cache
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
	require.NoError(t, seedCache(t, dir, store, legacyKey, "legacy-tok", time.Now().Add(time.Hour), "prod", "host", "c-1"))
	// Ask the fast path directly: a broken config.yaml used to stand in
	// for "the rest of the command cannot run", and no longer can - it
	// makes every cache hit a miss, which would pass this assertion for
	// the wrong reason.
	cmd := tokenCmd{dir: dir, store: store, anchor: anchorFor(dir)}
	_, _, _, hit := cmd.probeFlagCache("prod", "host", "c-1", "", "", ui.Discard())
	assert.False(t, hit, "schemeless --server must not serve a legacy host-only cache record")
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
	require.NoError(t, seedCache(t, dir, store, httpsKey, "https-tok", time.Now().Add(time.Hour), "prod", "https://host", "c-1"))
	// A hit short-circuits before any connection, which is what makes
	// this assertion about normalisation alone: the full path would have
	// to reach the network to produce a token at all.

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "host", "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "https-tok", cred.Status.Token, "schemeless --server must hit the current https:// cache record")
}

// TestGetTokenCreatesFromStoredLoginCredential asserts that on
// a cache miss, get-token must create a cluster-scoped token from the stored
// login credential for the matching configured server BEFORE ever touching
// the interactive provider flow. authProviderHits stays at 0 to prove the
// interactive path was provably not invoked.
func TestGetTokenCreatesFromStoredLoginCredential(t *testing.T) {
	var authProviderHits, derivedCreateHits atomic.Int32
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1-public/authproviders"):
			authProviderHits.Add(1)
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == ExtTokensPath && r.Method == http.MethodPost:
			derivedCreateHits.Add(1)
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"created"},"status":{"bearerToken":"ext/created:secret"}}`))
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

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "ext/created:secret", cred.Status.Token)
	assert.Equal(t, int32(1), derivedCreateHits.Load())
	assert.Equal(t, "Bearer stored-login-token", gotAuth, "the derived token create request must bear the stored login token, not a fresh session token")
	assert.Equal(t, int32(0), authProviderHits.Load(), "the interactive provider flow must not run when the stored credential creates successfully")
}

// TestGetTokenFallsBackToInteractiveWhenCreateRejected covers the "create is
// rejected" half of the fallback: a stored credential exists but the derived
// token create call fails, so get-token must still fall through to the
// interactive flow (observed here as an authproviders hit) rather than
// surfacing the derived token create error directly.
// TestGetTokenLegacyFallbackCachesWithBoundedTTL asserts that a legacy
// (pre-ext.cattle.io) Rancher's stored-credential fallback is still cached -
// with a bounded, conservative TTL, since the raw stored token carries no
// expiry of its own - rather than never (a zero expiry trips the
// no-zero-expiry-cache gate, forcing a full create-then-404 round trip on every
// single invocation).
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

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "stored-login-token", cred.Status.Token)

	_, expiry, cerr := store.GetCachedCred(config.CacheKey("prod", config.CanonicalServerIdentity(srv.URL), "local"), anchorFor(dir))
	require.NoError(t, cerr, "the legacy fallback must be cached, not treated as zero-expiry")
	assert.WithinDuration(t, time.Now().Add(legacyTokenTTL), expiry, time.Minute)
}

// TestGetTokenFallsBackToInteractiveWhenCreateRejected asserts the 401 half
// of the fallback: a stored credential the server no longer accepts runs the
// interactive flow. A 403 is a different rule; see
// TestGetTokenCreateForbiddenIsRBACNotInteractive.
func TestGetTokenFallsBackToInteractiveWhenCreateRejected(t *testing.T) {
	var authProviderHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1-public/authproviders"):
			authProviderHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"pingProvider"}]}`))
		case r.URL.Path == ExtTokensPath && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusUnauthorized)
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

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.Discard())
	// SAML under a non-interactive prompter fast-fails AUTH - the point here
	// is that the flow reached the provider dance at all.
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, int32(1), authProviderHits.Load(), "a rejected create must fall through to the interactive flow")
}

// TestGetTokenCreateTimeoutSurfacesAsGenericTransportError asserts a stalled
// create request is bounded by httpx's client-level timeout rather than
// hanging forever, and that the resulting error keeps CreateDerivedToken's
// existing classification: a *clierr.Error with CodeGeneric, the same shape
// any other transport failure at this call site already takes (a timeout is
// neither rejection sentinel, so it must NOT fall back to the interactive flow the
// way a rejected 401/403 create does).
func TestGetTokenCreateTimeoutSurfacesAsGenericTransportError(t *testing.T) {
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
		case r.URL.Path == ExtTokensPath && r.Method == http.MethodPost:
			<-block
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	// close(block) before srv.Close(): Close waits for in-flight handlers to
	// return, and the derived token create handler is still parked on <-block after the
	// client gives up - closing the server first would deadlock.
	defer func() { close(block); srv.Close() }()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	start := time.Now()
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.Discard())
	elapsed := time.Since(start)

	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Equal(t, int32(0), authProviderHits.Load(), "a timeout is neither rejection sentinel and must not trigger the interactive fallback")
	assert.Less(t, elapsed, 5*time.Second, "the derived token create request must be bounded by the client timeout, not hang on the blocked handler")
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

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "c-1"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, int32(1), authProviderHits.Load(), "no stored credential must fall through to the interactive flow")
}

// TestGetTokenCachesCreatedExpiry asserts that the
// stored-credential create's status.expiresAt is propagated into the
// cached credential's real expiry, not silently dropped.
func TestGetTokenCachesCreatedExpiry(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ExtTokensPath && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"metadata":{"name":"created"},"status":{"bearerToken":"ext/created:secret","expiresAt":%q}}`, expiresAt)
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

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.Discard())
	require.NoError(t, err)

	_, gotExpiry, cerr := store.GetCachedCred(config.CacheKey("prod", config.CanonicalServerIdentity(srv.URL), "local"), anchorFor(dir))
	require.NoError(t, cerr, "a token with a real expiry must be cached")
	assert.WithinDuration(t, time.Now().Add(time.Hour), gotExpiry, time.Minute)
}

// TestGetTokenSkipsCacheWhenCreateCarriesNoExpiry asserts that a derived token
// create response with no expiresAt is never cached - a zero expiry reads back
// as never-expiring (GetCachedCred only evicts a past, non-zero expiry), so
// caching it would serve a stale token forever.
func TestGetTokenSkipsCacheWhenCreateCarriesNoExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ExtTokensPath && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"created"},"status":{"bearerToken":"ext/created:secret"}}`))
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
	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.New(&notices, strings.NewReader(""), true, false))
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "ext/created:secret", cred.Status.Token, "the token is still usable for this call")

	_, _, cerr := store.GetCachedCred(config.CacheKey("prod", config.CanonicalServerIdentity(srv.URL), "local"), anchorFor(dir))
	require.ErrorIs(t, cerr, config.ErrNoToken, "a zero-expiry token must not be cached")
	assert.Contains(t, notices.String(), "not caching")
}

// TestZeroExpiryCachedWhenAllowed mirrors TestGetTokenSkipsCacheWhenCreateCarriesNoExpiry
// but runs as the legacy shim (legacyShim, the field that opts into zero-expiry
// caching). The credential must be written to the store (zero expiry =
// never-expiring on the legacy path), and the emitted ExecCredential must
// carry no ExpirationTimestamp.
func TestZeroExpiryCachedWhenAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ExtTokensPath && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"created"},"status":{"bearerToken":"ext/created:secret"}}`))
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

	// The shim's cache slot, as shim.go names it for --user prod.
	slot := config.LegacyCachePrefix + "prod"
	cmd := tokenCmd{dir: dir, store: store}
	res, err := cmd.runWithParams(t.Context(), runParams{
		Server:     srv.URL,
		Name:       slot,
		legacyShim: true,
	}, ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "ext/created:secret", cred.Status.Token)
	assert.Nil(t, cred.Status.ExpirationTimestamp, "zero expiry must not set ExpirationTimestamp")

	tok, exp, cerr := store.GetCachedCred(config.CacheKey(slot, config.CanonicalServerIdentity(srv.URL), ""), anchorFor(dir))
	require.NoError(t, cerr, "zero-expiry credential must be cached on the legacy shim path")
	assert.Equal(t, "ext/created:secret", tok)
	assert.True(t, exp.IsZero(), "cached expiry must be zero")
}

// TestGetTokenCreateScopesToCluster asserts that --cluster is
// threaded into the derived token create request as spec.clusterName, so
// two different --cluster values produce two distinct scoped creates.
func TestGetTokenCreateScopesToCluster(t *testing.T) {
	var gotClusters []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == steveClustersPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(steveClustersBody([2]string{"c-a", ""}, [2]string{"c-b", ""}))
		case r.URL.Path == ExtTokensPath && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			gotClusters = append(gotClusters, createdClusterName(t, body))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"created"},"status":{"bearerToken":"ext/created:secret"}}`))
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

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
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

// TestGetTokenClusterDisplayNameResolves asserts that an unresolved display
// name passed to --cluster must resolve to the cluster's id (via the cluster
// list, using the stored credential) before creating - a raw, unresolved
// display name must never reach spec.clusterName.
func TestGetTokenClusterDisplayNameResolves(t *testing.T) {
	var gotCluster, gotAuth string
	var listHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == steveClustersPath:
			listHits.Add(1)
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(steveClustersBody([2]string{"c-m-abc", "prod"}))
		case r.URL.Path == ExtTokensPath && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			gotCluster = createdClusterName(t, body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"created"},"status":{"bearerToken":"ext/created:secret"}}`))
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

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "cluster": "prod"}},
		ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "c-m-abc", gotCluster, "the resolved id, not the raw display name, must reach the derived token create")
	assert.Equal(t, int32(1), listHits.Load())
	assert.Equal(t, "Bearer stored-login-token", gotAuth, "cluster resolution uses the stored credential, not a fresh session")
}

// TestGetTokenClusterMissErrorsWithoutCreating asserts the "unresolvable →
// propagate the resolution error" half: an unresolvable --cluster must fail
// directly, never create an unscoped token.
func TestGetTokenClusterMissErrorsWithoutCreating(t *testing.T) {
	var derivedCreateHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case steveClustersPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(steveClustersBody())
		case ExtTokensPath:
			derivedCreateHits.Add(1)
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

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "cluster": "missing"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, int32(0), derivedCreateHits.Load(), "an unresolvable cluster must never reach the derived token create call")
}

// TestGetTokenClusterIDWinsOverAnotherClustersDisplayName asserts a
// --cluster value that is one cluster's id and another cluster's display
// name creates the token for the id, and the warning naming the other
// cluster goes to stderr, never into the ExecCredential.
func TestGetTokenClusterIDWinsOverAnotherClustersDisplayName(t *testing.T) {
	var listHits atomic.Int32
	var gotCluster string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == steveClustersPath:
			listHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(steveClustersBody([2]string{"c-1", "prod"}, [2]string{"c-2", "c-1"}, [2]string{"c-xyz", "staging"}))
		case r.URL.Path == ExtTokensPath && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			gotCluster = createdClusterName(t, body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"created"},"status":{"bearerToken":"ext/created:secret"}}`))
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

	var stderr bytes.Buffer
	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "cluster": "c-1"}},
		ui.New(&stderr, strings.NewReader(""), true, false))
	require.NoError(t, err)
	assert.Equal(t, "c-1", gotCluster, "the id wins")
	assert.Equal(t, int32(1), listHits.Load())
	assert.Contains(t, stderr.String(), "--cluster 'c-1' is the id of cluster 'c-1' and the display name of cluster 'c-2'; the command uses the one with that id. To use the other one, pass --cluster 'c-2'")
	assert.Empty(t, res.Warnings, "the ExecCredential result carries no warning")
}

// TestGetTokenRejectsLegacyPrefixName asserts that the native
// get-token command rejects --name values starting with "legacy:" (they
// are reserved for the legacy token shim's cache slots).
func TestGetTokenRejectsLegacyPrefixName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := newTokenCommandForTest(t, dir, config.NewFileStore(dir)).(runtime.ReadCommand)
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
// entry must not be served - the VALIDATION error must surface even when the
// cache holds a valid token for that key. Without the run()-level guard this
// test fails because probeFlagCache returns a cache hit before runWithParams
// ever checks IsLegacyCacheSlot.
func TestGetTokenRejectsLegacyPrefixNameBeforeCacheLookup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	serverID := config.CanonicalServerIdentity("https://r.ex")
	key := config.CacheKey("legacy:foo", serverID, "")
	require.NoError(t, seedCache(t, dir, store, key, "warm-tok", time.Now().Add(time.Hour), "legacy:foo", serverID, ""))

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "https://r.ex", "name": "legacy:foo"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce, "must fail with a VALIDATION error")
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	require.ErrorContains(t, ce, "legacy:")

	// The cache entry must be untouched - it was never served.
	tok, _, cerr := store.GetCachedCred(key, anchorFor(dir))
	require.NoError(t, cerr, "cache entry must still be present")
	assert.Equal(t, "warm-tok", tok, "cache entry must be untouched")
}

// errCacheStore wraps a CredStore but returns a fixed error from GetCachedCred,
// so the double-notice test can inject a repeatable store failure without
// corrupting any other store method.
type errCacheStore struct {
	config.CredStore
	cacheErr error
}

func (s *errCacheStore) GetCachedCred(key string, _ *config.CacheAnchor) (string, time.Time, error) {
	return "", time.Time{}, s.cacheErr
}

// TestCacheReadErrorNoticedOnce verifies that a single get-token invocation
// with a store that errors on GetCachedCred produces the "cache read failed"
// notice exactly once - even when the resolved cache key differs from the
// fast-path probed key (which happens whenever the current context fills in a
// non-empty cluster). A fixture with no current-context
// cluster makes flagKey == cacheKey; the sub-test below exercises the case
// that fixture would mask.
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

// TestStoredTokenForServerIdentifiedEntryWinsOutright asserts that an
// exact --name match is a certainty: its credential is the one returned, and
// another entry on the URL that also holds a credential neither competes nor
// is mentioned.
func TestStoredTokenForServerIdentifiedEntryWinsOutright(t *testing.T) {
	store := config.NewFileStore(t.TempDir())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod-a"}, "tok-a"))
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod-z"}, "tok-z"))
	cfg := &config.Config{
		Servers: map[string]config.Server{
			"prod-a": {URL: "https://r.ex"},
			"prod-z": {URL: "https://r.ex"},
		},
	}

	got, ok, serr := lookupStored(cfg, store, "https://r.ex", "prod-a", "", false)
	require.NoError(t, serr)
	require.True(t, ok)
	assert.Equal(t, "prod-a", got.name)
	assert.Equal(t, "tok-a", got.token)
}

// TestGetTokenSurfacesRealCreateFailureNotInteractive asserts that a non-auth
// create failure (a 500 here, standing in for network/TLS failures too) must
// surface as the real error, never papered over as "interactive login
// required".
func TestGetTokenSurfacesRealCreateFailureNotInteractive(t *testing.T) {
	var authProviderHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1-public/authproviders"):
			authProviderHits.Add(1)
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == ExtTokensPath && r.Method == http.MethodPost:
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

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.Discard())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "interactive login required")
	require.ErrorContains(t, err, `creating derived token: an error on the server ("Internal Server Error") has prevented the request from succeeding - retrying may succeed`)
	ce := clierr.From(err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.True(t, ce.Retryable, "a 5xx that is not a Status answer is retryable")
	assert.Equal(t, int32(0), authProviderHits.Load(), "a real create failure must not fall through to the interactive flow")
}

// TestGetTokenNoticesUnreadableConfigOnStoredCredPath asserts that
// a config.yaml the stored-credential path can't parse must
// be noticed, not silently folded into a generic "requires login" error.
func TestGetTokenNoticesUnreadableConfigOnStoredCredPath(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("\t: not yaml ["), 0o600))

	var notices bytes.Buffer
	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	// --cluster and a valid (httpx.New parses it into a cert pool) --cacert
	// are set explicitly so resolveConn's OWN config.yaml read is skipped
	// (all three of server/cluster/caIn are non-empty) - the point of this
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
	require.Error(t, err, "no stored credential and no reachable interactive provider - some error is expected")
	assert.Contains(t, notices.String(), "config unreadable")
}

// TestStoredTokenForServerTieBreak asserts how two entries sharing a URL
// resolve when the call names neither: the current server wins, and with no
// current server the lookup refuses to pick one.
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

	t.Run("no current server is an error naming both entries", func(t *testing.T) {
		_, ok, serr := lookupStored(cfg, store, "https://r.ex", "", "c-1", false)
		assert.False(t, ok)
		ce, isCE := errors.AsType[*clierr.Error](serr)
		require.True(t, isCE, "%v", serr)
		assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
		assert.Equal(t, []string{"prod-a", "prod-b"}, candidateIDsToken(ce.Candidates), "the entries are named in name order")
	})

	t.Run("the current server wins the tie, regardless of name order", func(t *testing.T) {
		cfg.CurrentServer = "prod-b"
		got, ok, serr := lookupStored(cfg, store, "https://r.ex", "", "", false)
		require.NoError(t, serr)
		require.True(t, ok)
		assert.Equal(t, "prod-b", got.name)
		assert.Equal(t, "tok-b", got.token)
	})
}

// TestStoredTokenForServerExhaustsCandidatesWithNoToken asserts that the
// tie-break's chosen candidate having no stored
// token must not mask a sibling that does - every candidate is tried, not
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
	got, ok, serr := lookupStored(cfg, store, "https://r.ex", "", "", false)
	require.NoError(t, serr)
	require.True(t, ok, "a tokenless top-ranked candidate must not short-circuit the search")
	assert.Equal(t, "prod-b", got.name)
	assert.Equal(t, "tok-b", got.token)
}

// TestStoredTokenForServerStopsOnACredentialIdentityError asserts that
// anything the identity layer says about an entry ends the candidate
// search: a sibling sharing the URL belongs to another user, and using it
// would authenticate as that user and swallow the message. The three
// cases are the three shapes that layer produces - a credential another
// directory replaced, a login that did not settle, and a record this
// directory can no longer read.
func TestStoredTokenForServerStopsOnACredentialIdentityError(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"replaced by another directory", clierr.New(clierr.CodeCredentialReplaced, "the stored credential for 'prod-a' was replaced").WithCause(config.ErrCredentialIdentity)},
		{"a login that did not settle", clierr.Retryable(clierr.CodeLoginIncomplete, "a login for 'prod-a' is in progress").WithCause(config.ErrCredentialIdentity)},
		{"a record that cannot be read", clierr.New(clierr.CodeValidation, "the credential identity record for 'prod-a' is unreadable").WithCause(config.ErrCredentialIdentity)},
	}
	// prod-b answers in both shapes. With no current server it ties prod-a's
	// rank, where a read failure of any kind would be returned anyway. With
	// prod-b current it is a clear winner ranked above prod-a, where only
	// the identity rule can return the error.
	for _, tc := range cases {
		for _, current := range []string{"", "prod-b"} {
			t.Run(tc.name+", current server "+current, func(t *testing.T) {
				base := config.NewFileStore(t.TempDir())
				require.NoError(t, base.SetToken(config.ServerRef{Name: "prod-b"}, "tok-b"))
				store := entryErrStore{CredStore: base, entry: "prod-a", err: tc.err}
				cfg := &config.Config{CurrentServer: current, Servers: map[string]config.Server{
					"prod-a": {URL: "https://r.ex"},
					"prod-b": {URL: "https://r.ex"},
				}}

				got, ok, serr := lookupStored(cfg, store, "https://r.ex", "", "", false)
				require.ErrorIs(t, serr, tc.err)
				assert.False(t, ok, "the matching sibling is never used instead")
				assert.Empty(t, got.token)
			})
		}
	}
}

// entryErrStore answers one entry with a fixed error and forwards every
// other read to a real store.
type entryErrStore struct {
	config.CredStore
	entry string
	err   error
}

func (s entryErrStore) GetToken(ref config.ServerRef) (string, error) {
	if ref.Name == s.entry {
		return "", s.err
	}
	return s.CredStore.GetToken(ref)
}

// TestIsLoginIncomplete asserts what the lane decision branches on: the
// unsettled login alone, and nothing else mistaken for it - a credential
// another directory replaced least of all, since treating that as a miss
// would start a login on top of it.
func TestIsLoginIncomplete(t *testing.T) {
	t.Parallel()
	assert.True(t, isLoginIncomplete(clierr.Retryable(clierr.CodeLoginIncomplete, "x")))
	assert.False(t, isLoginIncomplete(clierr.New(clierr.CodeCredentialReplaced, "x")))
	assert.False(t, isLoginIncomplete(clierr.New(clierr.CodeAuth, "x")))
	assert.False(t, isLoginIncomplete(config.ErrNoToken))
	assert.False(t, isLoginIncomplete(nil))
}

// TestStoredTokenForServerUnreachableStoreDecidesOnlyBelowTheWinner asserts
// the other half of the identity rule: a store that could not be reached
// says nothing about who an entry is, but it leaves unknown whether that
// entry holds a credential. A read that fails at or above the rank of the
// entry that answers is returned, never answered around; one below it
// cannot change the choice and does not block a clear winner. With no
// entry answering, the failure surfaces.
func TestStoredTokenForServerUnreachableStoreDecidesOnlyBelowTheWinner(t *testing.T) {
	unreachable := errors.New(`keyring get "prod-a": dbus: no session bus`)
	newCfg := func(current string) *config.Config {
		return &config.Config{CurrentServer: current, Servers: map[string]config.Server{
			"prod-a": {URL: "https://r.ex"},
			"prod-b": {URL: "https://r.ex"},
		}}
	}
	base := config.NewFileStore(t.TempDir())
	require.NoError(t, base.SetToken(config.ServerRef{Name: "prod-b"}, "tok-b"))
	store := entryErrStore{CredStore: base, entry: "prod-a", err: unreachable}

	t.Run("at the winning rank, with no current server", func(t *testing.T) {
		_, ok, serr := lookupStored(newCfg(""), store, "https://r.ex", "", "", false)
		assert.False(t, ok)
		require.ErrorIs(t, serr, unreachable)
	})
	t.Run("above the entry that answers: the current server is unreadable", func(t *testing.T) {
		_, ok, serr := lookupStored(newCfg("prod-a"), store, "https://r.ex", "", "", false)
		assert.False(t, ok)
		require.ErrorIs(t, serr, unreachable)
	})
	t.Run("below the winner: the current server answers", func(t *testing.T) {
		got, ok, serr := lookupStored(newCfg("prod-b"), store, "https://r.ex", "", "", false)
		require.NoError(t, serr)
		require.True(t, ok)
		assert.Equal(t, "tok-b", got.token)
	})
	t.Run("two unreadable entries either side of the winner: the lowest-ranked failure is the one compared", func(t *testing.T) {
		high := errors.New(`keyring get "prod-a": dbus: no session bus`)
		low := errors.New(`keyring get "exec-login-r": helper timed out`)
		cfg := &config.Config{CurrentServer: "prod-a", TokenReuse: true, Servers: map[string]config.Server{
			"prod-a":       {URL: "https://r.ex"},
			"prod-b":       {URL: "https://r.ex"},
			"exec-login-r": {URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin},
		}}
		unreadableHigh := config.CredStore(entryErrStore{CredStore: base, entry: "prod-a", err: high})
		both := entryErrStore{CredStore: unreadableHigh, entry: "exec-login-r", err: low}
		_, ok, serr := lookupStored(cfg, both, "https://r.ex", "", "", false)
		assert.False(t, ok, "prod-b is not chosen over the unreadable current server")
		require.ErrorIs(t, serr, high)
		require.NotErrorIs(t, serr, low)
	})
	t.Run("no entry answers", func(t *testing.T) {
		cfg := newCfg("")
		delete(cfg.Servers, "prod-b")
		_, ok, serr := lookupStored(cfg, store, "https://r.ex", "", "", false)
		assert.False(t, ok)
		require.ErrorIs(t, serr, unreachable)
	})
}

// TestStoredTokenForServerNamePicksNamedServer asserts that an
// explicit --name must win over the lexicographic default,
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
	got, ok, serr := lookupStored(cfg, store, "https://r.ex", "prod-b", "", false)
	require.NoError(t, serr)
	require.True(t, ok)
	assert.Equal(t, "prod-b", got.name, "--name must win over the lexicographic default")
	assert.Equal(t, "tok-b", got.token)
}

// TestStoredTokenForServerIgnoreCurrentContextSkipsLegacyNamedEntry asserts
// that a config.yaml entry literally named "legacy:<user>" (a hand-edited
// file, or one written before auth login rejected the prefix) is not the
// entry a shim call means. The shim's name is a cache-slot label, so it
// identifies nothing: with the entry the only one holding a credential it is
// used like any other candidate, and with a second one holding a credential
// the lookup refuses to choose.
func TestStoredTokenForServerIgnoreCurrentContextSkipsLegacyNamedEntry(t *testing.T) {
	store := config.NewFileStore(t.TempDir())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "legacy:zed"}, "tok-legacy"))
	cfg := &config.Config{
		Servers: map[string]config.Server{
			"legacy:zed": {URL: "https://r.ex"},
			"aaa":        {URL: "https://r.ex"},
		},
	}
	identified, err := identifyEntry(cfg, "https://r.ex", "legacy:zed", "", true)
	require.NoError(t, err)
	assert.Empty(t, identified, "a shim call never identifies an entry")

	got, ok, serr := lookupStored(cfg, store, "https://r.ex", "legacy:zed", "", true)
	require.NoError(t, serr)
	require.True(t, ok)
	assert.Equal(t, "legacy:zed", got.name, "the only entry with a credential is used")

	require.NoError(t, store.SetToken(config.ServerRef{Name: "aaa"}, "tok-aaa"))
	_, ok, serr = lookupStored(cfg, store, "https://r.ex", "legacy:zed", "", true)
	assert.False(t, ok)
	ce, isCE := errors.AsType[*clierr.Error](serr)
	require.True(t, isCE, "%v", serr)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Equal(t, []string{"aaa", "legacy:zed"}, candidateIDsToken(ce.Candidates), "the entries are named in name order")
}

// TestStoredTokenForServerShimIgnoresTheCurrentServer asserts the shim is
// driven by its flags alone: with two readable entries on the call's URL,
// one of them the directory's current server, a native call uses the current
// entry's credential while the shim gets the tie error and neither token.
func TestStoredTokenForServerShimIgnoresTheCurrentServer(t *testing.T) {
	store := config.NewFileStore(t.TempDir())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod-a"}, "tok-a"))
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod-b"}, "tok-b"))
	cfg := &config.Config{
		CurrentServer: "prod-b",
		Servers: map[string]config.Server{
			"prod-a": {URL: "https://r.ex"},
			"prod-b": {URL: "https://r.ex"},
		},
	}

	got, ok, serr := lookupStored(cfg, store, "https://r.ex", "", "c-1", false)
	require.NoError(t, serr)
	require.True(t, ok)
	assert.Equal(t, "tok-b", got.token, "a native call prefers the current server")

	got, ok, serr = lookupStored(cfg, store, "https://r.ex", "legacy:user", "c-1", true)
	assert.False(t, ok)
	assert.Empty(t, got.token, "the shim serves neither entry's token")
	ce, isCE := errors.AsType[*clierr.Error](serr)
	require.True(t, isCE, "%v", serr)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Equal(t, []string{"prod-a", "prod-b"}, candidateIDsToken(ce.Candidates), "the entries are named in name order")
}

// TestStoredTokenForServerMatchesEquivalentURLSpellings asserts that
// candidate matching uses canonical server identity, so
// a configured "https://host" is found for a "HTTPS://HOST:443" query (case
// and an explicit default port are irrelevant) rather than skipped.
func TestStoredTokenForServerMatchesEquivalentURLSpellings(t *testing.T) {
	store := config.NewFileStore(t.TempDir())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "tok"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://host"}}}

	got, ok, serr := lookupStored(cfg, store, "HTTPS://HOST:443", "", "", false)
	require.NoError(t, serr)
	require.True(t, ok, "an equivalent URL spelling must still match the configured server")
	assert.Equal(t, "prod", got.name)
	assert.Equal(t, "tok", got.token)
}

func TestSelectAuthProviderExplicitMatch(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&LocalProvider{AuthProvider{Type: "localProvider"}}, &AuthProvider{Type: "pingProvider"}}
	got, err := selectAuthProvider(t.Context(), ps, nil, "pingProvider", false, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "pingProvider", got.GetType())
}

func TestSelectAuthProviderNotFound(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&AuthProvider{Type: "pingProvider", ID: "ping"}}
	_, err := selectAuthProvider(t.Context(), ps, nil, "nope", false, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, "--auth-provider nope matches no auth config this CLI can sign in with on this server", ce.Message)
	assert.Equal(t, []string{"ping"}, candidateIDs(ce.Candidates))
}

// TestSelectAuthProviderMatchesTheProviderID asserts --auth-provider accepts
// an auth config's name (local) and the provider type (localProvider).
func TestSelectAuthProviderMatchesTheProviderID(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&LocalProvider{AuthProvider{Type: "localProvider", ID: "local"}}, &AuthProvider{Type: "pingProvider", ID: "ping"}}
	got, err := selectAuthProvider(t.Context(), ps, nil, "ping", false, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "pingProvider", got.GetType())
	got, err = selectAuthProvider(t.Context(), ps, nil, "localProvider", false, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "local", got.GetID())
}

// TestSelectAuthProviderNotFoundNamesUnsupported asserts a miss names the
// providers the server advertises that this CLI cannot log in with, with
// the server change requested, while the candidates stay the supported
// auth configs only.
func TestSelectAuthProviderNotFoundNamesUnsupported(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&LocalProvider{AuthProvider{Type: "localProvider", ID: "local"}}}
	_, err := selectAuthProvider(t.Context(), ps, []string{"githubProvider"}, "github", false, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Contains(t, ce.Message, "--auth-provider github matches no auth config this CLI can sign in with on this server; advertised by the server but not supported by this CLI: githubProvider (server change requested: rancher/rancher#57034)")
	assert.Equal(t, []string{"local"}, candidateIDs(ce.Candidates))
}

// TestSelectAuthProviderAmbiguousNamesUnsupported asserts the no-terminal
// refusal between several supported auth configs still names the advertised
// types this CLI cannot use.
func TestSelectAuthProviderAmbiguousNamesUnsupported(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&AuthProvider{Type: "pingProvider", ID: "ping"}, &AuthProvider{Type: "oktaProvider", ID: "okta"}}
	_, err := selectAuthProvider(t.Context(), ps, []string{"githubProvider"}, "", false, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Contains(t, ce.Message, "not supported by this CLI: githubProvider")
	assert.ElementsMatch(t, []string{"ping", "okta"}, candidateIDs(ce.Candidates))
}

// TestSelectAuthProviderSingleNoticesUnsupported asserts the one supported
// provider is chosen without a prompt, but the unsupported ones are stated
// first when a terminal is there to read them, and never on --no-input.
func TestSelectAuthProviderSingleNoticesUnsupported(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&LocalProvider{AuthProvider{Type: "localProvider", ID: "local"}}}
	var errBuf strings.Builder
	p := ui.New(&errBuf, blockingReader{}, false, true)
	got, err := selectAuthProvider(t.Context(), ps, []string{"githubProvider"}, "", false, p)
	require.NoError(t, err)
	assert.Equal(t, "localProvider", got.GetType())
	assert.Contains(t, errBuf.String(), "Auth config: local")
	assert.Contains(t, errBuf.String(), "not supported by this CLI: githubProvider")

	var quiet strings.Builder
	_, err = selectAuthProvider(t.Context(), ps, []string{"githubProvider"}, "", false, ui.New(&quiet, blockingReader{}, true, false))
	require.NoError(t, err)
	assert.Empty(t, quiet.String(), "no prompt, no notice")
}

// TestGetAuthProvidersSplitsSupportedAndUnsupported asserts the server's
// list is split: supported providers decoded with their ids, local first;
// unsupported types returned sorted and deduplicated.
func TestGetAuthProvidersSplitsSupportedAndUnsupported(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"github","type":"githubProvider"},{"id":"okta","type":"oktaProvider"},{"id":"local","type":"localProvider"},{"id":"google","type":"googleOAuthProvider"},{"id":"github2","type":"githubProvider"}]}`))
	}))
	defer srv.Close()
	supported, unsupported, _, err := getAuthProviders(t.Context(), srv.Client(), srv.URL)
	require.NoError(t, err)
	require.Len(t, supported, 2)
	assert.Equal(t, "localProvider", supported[0].GetType(), "local first")
	assert.Equal(t, "local", supported[0].GetID())
	assert.Equal(t, "oktaProvider", supported[1].GetType())
	assert.Equal(t, []string{"githubProvider", "googleOAuthProvider"}, unsupported)
}

// TestGetAuthProvidersDecodesTheList asserts how getAuthProviders decodes
// the server's provider list: the providers this CLI supports, local first;
// the unsupported types sorted and deduplicated; their ids in response
// order; keys matched as encoding/json matches them (the last
// case-insensitive match), the same rule the provider decode uses; and an
// error for a list that does not decode - a body that is not JSON or has
// data after the list, a data member that is not an array, an element
// that is not an object, a type or id that is not a string, or a
// supported provider whose own fields do not decode. Each row's failsIf
// names a change to the decoding that the row catches.
func TestGetAuthProvidersDecodesTheList(t *testing.T) {
	t.Parallel()
	const listErr = "error decoding the auth provider list"
	cases := []struct {
		name            string
		failsIf         string
		body            string
		wantSupported   []string
		wantTypes       []string
		wantUnsupported []string
		wantIDs         []string
		wantErr         string
	}{
		{name: "data absent", failsIf: "an absent data member is an error", body: `{}`},
		{name: "data null", failsIf: "a null data member is an error", body: `{"data":null}`},
		{name: "null element is skipped", failsIf: "a null element is an error or a provider", body: `{"data":[null,{"id":"local","type":"localProvider"}]}`, wantSupported: []string{"local"}, wantTypes: []string{"localProvider"}},
		{name: "element without a type is skipped", failsIf: "an element without a type is listed", body: `{"data":[{"id":"x"},{"id":"local","type":"localProvider"}]}`, wantSupported: []string{"local"}, wantTypes: []string{"localProvider"}},
		{name: "local provider first", failsIf: "localProvider is not moved first", body: `{"data":[{"id":"okta","type":"oktaProvider"},{"id":"local","type":"localProvider"}]}`, wantSupported: []string{"local", "okta"}, wantTypes: []string{"localProvider", "oktaProvider"}},
		{name: "last case-insensitive type key counts", failsIf: "the type is read from a different key than the provider decode uses", body: `{"data":[{"id":"l","type":"githubProvider","TYPE":"localProvider"}]}`, wantSupported: []string{"l"}, wantTypes: []string{"localProvider"}},
		{name: "last case-insensitive type key counts, reversed", failsIf: "the type is read from a different key than the provider decode uses", body: `{"data":[{"id":"l","type":"localProvider","TYPE":"githubProvider"}]}`, wantUnsupported: []string{"githubProvider"}, wantIDs: []string{"l"}},
		{name: "last case-insensitive data key counts", failsIf: "the list is read from a different key than encoding/json uses", body: `{"data":[{"id":"local","type":"localProvider"}],"DATA":null}`},
		{
			name:            "Rancher-shaped list with members the CLI does not read",
			failsIf:         "members the CLI does not read are refused",
			body:            `{"type":"collection","links":{"self":"https://r/v1-public/authproviders"},"actions":{},"pagination":{"limit":1000},"resourceType":"authProvider","data":[{"id":"local","type":"localProvider","baseType":"authProvider","links":{"self":"https://r/v1-public/authproviders/local"},"actions":{"login":"https://r/v1-public/authproviders/local?action=login"}},{"id":"github","type":"githubProvider","baseType":"authProvider","links":{},"actions":{},"redirectUrl":"https://github.com/login/oauth/authorize"}]}`,
			wantSupported:   []string{"local"},
			wantTypes:       []string{"localProvider"},
			wantUnsupported: []string{"githubProvider"},
			wantIDs:         []string{"github"},
		},
		{
			name:            "unsupported types sorted and deduplicated",
			failsIf:         "the unsupported types are not sorted or not deduplicated",
			body:            `{"data":[{"id":"g1","type":"githubProvider"},{"id":"go","type":"googleOAuthProvider"},{"id":"g2","type":"githubProvider"}]}`,
			wantUnsupported: []string{"githubProvider", "googleOAuthProvider"},
			wantIDs:         []string{"g1", "go", "g2"},
		},
		{
			name:            "unsupported ids keep response order",
			failsIf:         "the unsupported ids are sorted, deduplicated or dropped, or an empty id is listed",
			body:            `{"data":[{"id":"g2","type":"githubProvider"},{"id":"okta","type":"oktaProvider"},{"type":"githubProvider"},{"id":"g1","type":"githubProvider"}]}`,
			wantSupported:   []string{"okta"},
			wantTypes:       []string{"oktaProvider"},
			wantUnsupported: []string{"githubProvider"},
			wantIDs:         []string{"g2", "g1"},
		},
		{name: "body that is not JSON", failsIf: "a list that does not decode is read", body: `{"data":[`, wantErr: listErr},
		{name: "page that is not the API names the remedy", failsIf: "the list error does not point at the server URL", body: `<html>`, wantErr: "; check the server URL"},
		{name: "data after the list", failsIf: "data after the list is ignored", body: `{"data":[{"id":"local","type":"localProvider"}]} x`, wantErr: listErr},
		{name: "top level an array", failsIf: "a list that does not decode is read", body: `[{"id":"local","type":"localProvider"}]`, wantErr: listErr},
		{name: "data an object", failsIf: "a list that does not decode is read", body: `{"data":{"id":"local","type":"localProvider"}}`, wantErr: listErr},
		{name: "data a scalar", failsIf: "a list that does not decode is read", body: `{"data":"local"}`, wantErr: listErr},
		{name: "element a scalar", failsIf: "an element that does not decode is skipped", body: `{"data":["x",{"id":"local","type":"localProvider"}]}`, wantErr: listErr},
		{name: "number type", failsIf: "an element that does not decode is skipped", body: `{"data":[{"id":"n","type":5}]}`, wantErr: listErr},
		{name: "bool id", failsIf: "an element that does not decode is skipped", body: `{"data":[{"id":true,"type":"githubProvider"}]}`, wantErr: listErr},
		{name: "supported provider whose fields do not decode", failsIf: "a decode error of a supported provider is ignored", body: `{"data":[{"id":"azure","type":"azureADProvider","redirectUrl":5}]}`, wantErr: "error decoding the auth provider azureADProvider"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			supported, unsupported, ids, err := getAuthProviders(t.Context(), srv.Client(), srv.URL)
			if c.wantErr != "" {
				require.ErrorContains(t, err, c.wantErr, "fails if %s", c.failsIf)
				return
			}
			require.NoError(t, err, "fails if %s", c.failsIf)
			var gotSupported, gotTypes []string
			for _, p := range supported {
				gotSupported = append(gotSupported, p.GetID())
				gotTypes = append(gotTypes, p.GetType())
			}
			assert.Equal(t, c.wantSupported, gotSupported, "supported; fails if %s", c.failsIf)
			assert.Equal(t, c.wantTypes, gotTypes, "supported types; fails if %s", c.failsIf)
			assert.Equal(t, c.wantUnsupported, unsupported, "unsupported; fails if %s", c.failsIf)
			assert.Equal(t, c.wantIDs, ids, "unsupported ids; fails if %s", c.failsIf)
		})
	}
}

// TestGetAuthProvidersDecodeErrorsKeepTheCause asserts that both decode
// errors of the provider list - the list itself and one element - name the
// remedy and wrap encoding/json's error, so a caller can still inspect it.
func TestGetAuthProvidersDecodeErrorsKeepTheCause(t *testing.T) {
	t.Parallel()
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	cases := []struct {
		name  string
		body  string
		cause any
	}{
		{name: "list", body: `<html>`, cause: &syntaxErr},
		{name: "element", body: `{"data":["x"]}`, cause: &typeErr},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			_, _, _, err := getAuthProviders(t.Context(), srv.Client(), srv.URL)
			require.ErrorContains(t, err, "error decoding the auth provider list: ")
			require.ErrorContains(t, err, "; check the server URL")
			assert.ErrorAs(t, err, c.cause, "the encoding/json error stays wrapped")
		})
	}
}

func TestSelectAuthProviderSingle(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&AuthProvider{Type: "pingProvider"}}
	got, err := selectAuthProvider(t.Context(), ps, nil, "", false, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "pingProvider", got.GetType())
}

func TestSelectAuthProviderAmbiguousNoInput(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&AuthProvider{Type: "pingProvider", ID: "ping"}, &AuthProvider{Type: "oktaProvider", ID: "okta"}}
	_, err := selectAuthProvider(t.Context(), ps, nil, "", false, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Equal(t, "this server has several auth configs: ping, okta; pass one of them with --auth-provider", ce.Message)
	assert.ElementsMatch(t, []string{"ping", "okta"}, candidateIDs(ce.Candidates))
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
	cmd := newTokenCommandForTest(t, dir, config.NewFileStore(dir)).(runtime.ReadCommand)
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
		assert.Empty(t, ca, "no configured entry matches - never borrow the current server's CA for a different server")
	})

	t.Run("empty cluster is a validation error on the native path", func(t *testing.T) {
		_, _, _, err := cmd.resolveConn("https://config.example", "", "", false)
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
		assert.ErrorContains(t, ce, "--cluster is required")
	})

	t.Run("empty cluster on shim path does not error", func(t *testing.T) {
		// ignoreCurrentContext=true (shim path): empty cluster stays unscoped, no error.
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
	cmd := newTokenCommandForTest(t, dir, config.NewFileStore(dir)).(runtime.ReadCommand)
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
	cmd := newTokenCommandForTest(t, dir, config.NewFileStore(dir)).(runtime.ReadCommand)
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
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1-public/authproviders"):
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"}]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1-public/login"):
			// A login that answers with an ext token: the sign-in's own token.
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"apiVersion": "ext.cattle.io/v1",
				"kind": "Token",
				"metadata": {"name": "token-login"},
				"status": {"bearerToken": "ext/token-login:lllll", "expiresAt": ""}
			}`))
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{
				"apiVersion": "ext.cattle.io/v1",
				"kind": "Token",
				"metadata": {"name": "token-e2e"},
				"spec": {"clusterName": "local"},
				"status": {"bearerToken": "ext/token-e2e:sssss", "expiresAt": ""}
			}`))
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	cmd := newTokenCommandForTest(t, dir, config.NewFileStore(dir)).(runtime.ReadCommand)
	var errBuf strings.Builder
	p := ui.New(&errBuf, newLineReader("alice", "secret"), false, true)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		p)
	require.NoError(t, err)

	cred := res.Data.(*execCredential)
	assert.Equal(t, "ext/token-e2e:sssss", cred.Status.Token,
		"ext bearer must reach the exec credential with its prefix intact")
}

func TestGetTokenCacheHitExtToken(t *testing.T) {
	// An ext-issued bearer ("ext/<name>:<value>") must round-trip the exec-cred
	// cache byte-for-byte - the ext/ prefix is opaque data.
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, seedCache(t, dir, store, config.CacheKey("prod", "https://r.ex", "c-1"), "ext/token-abc:secretxyz", time.Now().Add(time.Hour), "prod", "https://r.ex", "c-1"))
	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)

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
	cmd := newTokenCommandForTest(t, dir, config.NewFileStore(dir)).(runtime.ReadCommand)
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
	cmd := newTokenCommandForTest(t, dir, config.NewFileStore(dir)).(runtime.ReadCommand)
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
	cmd := newTokenCommandForTest(t, dir, config.NewFileStore(dir)).(runtime.ReadCommand)
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
	cmd := newTokenCommandForTest(t, dir, config.NewFileStore(dir)).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "c-1", "auth-flow": "authcode"}},
		p)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "interactive login required", "non-TTY must bypass the gate")
	assert.ErrorContains(t, err, "callback port", "should fail past the gate on the forced port error")
}

func TestOAuthAuthCodeAuthHonorsCallerContext(t *testing.T) {
	// Regression test for the exchange-ctx/rancherLogin-ctx shadowing bug: the
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
	// ctx. Ordering is deterministic - no goroutine race, no timing window.
	openBrowserStub := func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		cbURL := q.Get("redirect_uri") + "?code=fake-code&state=" + url.QueryEscape(q.Get("state"))
		resp, err := ownTransportClient(t).Get(cbURL)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		cancel()
		return nil
	}

	provider := &AzureADProvider{
		ClientID:      "test-client",
		Scopes:        []string{"openid"},
		AuthURL:       "https://idp.example/authorize",
		DeviceAuthURL: "https://idp.example/devicecode",
		TokenURL:      "https://idp.example/token",
	}
	input := &loginInput{server: rancherSrv.URL, authProvider: "azureADProvider", idpClient: ownTransportClient(t)}

	_, err := oauthAuthCodeAuth(ctx, rancherSrv.Client(), input, provider, 5*time.Second, openBrowserStub, ui.Discard())
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled, "error must wrap context.Canceled, got: %v", err)
	assert.Equal(t, int32(0), rancherHits.Load(),
		"Rancher login must never be reached once the caller context is cancelled")
}

func TestOAuthAuthCodeAuthAbortsOnCallerCancelDuringBrowserWait(t *testing.T) {
	// Regression test: cancellation while still WAITING for the browser
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

	// openBrowser stub: never drives the callback - cancels the caller ctx
	// immediately instead, so by the time the wait select runs, ctx.Done() is
	// already the only ready case (deterministic, no timing window).
	openBrowserStub := func(authURL string) error {
		cancel()
		return nil
	}

	provider := &AzureADProvider{
		ClientID:      "test-client",
		Scopes:        []string{"openid"},
		AuthURL:       "https://idp.example/authorize",  // never reached
		DeviceAuthURL: "https://idp.example/devicecode", // never reached
		TokenURL:      "https://idp.example/token",
	}
	input := &loginInput{server: rancherSrv.URL, authProvider: "azureADProvider", idpClient: ownTransportClient(t)}

	_, err := oauthAuthCodeAuth(ctx, rancherSrv.Client(), input, provider, 5*time.Second, openBrowserStub, ui.Discard())
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled, "error must wrap context.Canceled, got: %v", err)
	assert.Equal(t, int32(0), rancherHits.Load())
}

// The loopback callback must not reflect error/error_description
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
		resp, err := ownTransportClient(t).Get(cbURL)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err = io.ReadAll(resp.Body)
		return err
	}

	provider := &AzureADProvider{
		ClientID:      "test-client",
		Scopes:        []string{"openid"},
		AuthURL:       "https://idp.example/authorize",  // never reached
		DeviceAuthURL: "https://idp.example/devicecode", // never reached
		TokenURL:      "https://idp.example/token",
	}
	input := &loginInput{server: "http://ignored", authProvider: "azureADProvider", idpClient: ownTransportClient(t)}

	_, err := oauthAuthCodeAuth(t.Context(), failingTransportClient(t), input, provider, 5*time.Second, openBrowserStub, ui.Discard())
	require.Error(t, err)
	assert.NotContains(t, string(body), "<script>", "reflected markup must be escaped, got %q", body)
	assert.NotContains(t, string(body), "<img", "reflected markup must be escaped, got %q", body)
	assert.Contains(t, string(body), "&lt;script&gt;")
}

// An unsolicited request with the wrong (or no) state must not
// consume the callback's one-shot handler - otherwise the real IdP redirect
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
		attackResp, err := ownTransportClient(t).Get(attackURL)
		if err != nil {
			return err
		}
		_ = attackResp.Body.Close()
		if attackResp.StatusCode != http.StatusBadRequest {
			return errors.New("forged wrong-state request did not get 400 Bad Request")
		}

		cbURL := redirect + "?code=fake-code&state=" + url.QueryEscape(q.Get("state"))
		resp, err := ownTransportClient(t).Get(cbURL)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		return nil
	}

	provider := &AzureADProvider{
		ClientID:      "test-client",
		Scopes:        []string{"openid"},
		AuthURL:       idp.URL + "/authorize",
		DeviceAuthURL: idp.URL + "/devicecode",
		TokenURL:      idp.URL + "/token",
	}
	input := &loginInput{server: rancherSrv.URL, authProvider: "azureADProvider", idpClient: idp.Client()}

	var noticeBuf strings.Builder
	p := ui.New(&noticeBuf, strings.NewReader(""), true, false)

	tok, err := oauthAuthCodeAuth(t.Context(), rancherSrv.Client(), input, provider, 5*time.Second, openBrowserStub, p)
	require.NoError(t, err, "the real redirect must still be processed after a forged wrong-state request")
	require.NotNil(t, tok)
	assert.Contains(t, noticeBuf.String(), "invalid state",
		"a forged-only callback must not hang silently - the user must see why the wait continues")
}

// samlAuth's poll select needs a ctx.Done() case, like the OAuth
// authcode flow's equivalent select, so SIGTERM/caller cancellation is
// honored promptly rather than on the next poll tick. Shrinking the poll
// interval and cancelling well before the first tick shows it: without
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
		w.WriteHeader(http.StatusNotFound) // token never ready - keeps polling
	}))
	defer srv.Close()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		input := &loginInput{server: srv.URL}

		go func() {
			time.Sleep(50 * time.Millisecond) // fake clock - before the first 500ms tick
			cancel()
		}()

		start := time.Now()
		_, err := samlAuth(ctx, srv.Client(), input, ui.Discard())
		elapsed := time.Since(start)

		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled, "error must wrap context.Canceled, got: %v", err)
		// Fake time: RSA keygen and scheduling cost zero, so elapsed is the
		// cancel goroutine's 50ms exactly - anything at or past the tick
		// means the select waited for the poll instead of the cancellation.
		assert.Less(t, elapsed, samlPollInterval,
			"must abort on ctx cancellation, not wait for the next poll tick")
	})
}

// blockingReader never returns from Read, so readLine's scan goroutine cannot
// win the select against a cancelled context - making the ctrl-C test deterministic.
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
	_, err := selectAuthProvider(ctx, providers, nil, "", false, p)
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
	cmd := newTokenCommandForTest(t, dir, config.NewFileStore(dir)).(runtime.ReadCommand)
	var errBuf strings.Builder
	p := ui.New(&errBuf, newLineReader(), false, true)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "c-1"}},
		p)
	require.Error(t, err)
	require.ErrorContains(t, err, "error listing auth providers: the server could not find the requested resource; check the server URL",
		"a 404 from /v1-public/authproviders must surface as an error naming the server URL")
	ce := clierr.From(err)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.False(t, ce.Retryable)
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
		got := ApplyDefaultScheme(tc.in)
		assert.Equal(t, tc.want, got, "ApplyDefaultScheme(%q)", tc.in)
	}
}

// TestGetTokenHostPortWarmCacheHit verifies that a --server host:port flag
// (schemeless) normalizes to https://host:port and hits a warm cache entry.
func TestGetTokenHostPortWarmCacheHit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	key := config.CacheKey("prod", config.CanonicalServerIdentity("https://host:8443"), "c-1")
	require.NoError(t, seedCache(t, dir, store, key, "tok-8443", time.Now().Add(time.Hour), "prod", "https://host:8443", "c-1"))

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
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
	require.NoError(t, seedCache(t, dir, store, key, "tok-ipv6", time.Now().Add(time.Hour), "prod", "https://[::1]:8443", "c-1"))

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "[::1]:8443", "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "tok-ipv6", cred.Status.Token, "[::1]:port flag must hit the https:// warm cache record")
}

// TestGetTokenMalformedServerNoFastPathCacheHit verifies that a malformed
// --server value (e.g. https://[::1 - unclosed bracket) cannot produce a
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
	require.NoError(t, seedCache(t, dir, store, key, "should-not-be-served", time.Now().Add(time.Hour), "prod", fallbackID, "c-1"))
	cmd := tokenCmd{dir: dir, store: store, anchor: anchorFor(dir)}
	_, _, _, hit := cmd.probeFlagCache("prod", malformed, "c-1", "", "", ui.Discard())
	assert.False(t, hit, "malformed --server must not produce a fast-path cache hit")
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
	require.NoError(t, seedCache(t, dir, store, key, "should-not-be-served", time.Now().Add(time.Hour), "prod", ftpID, "c-1"))

	cmd := tokenCmd{dir: dir, store: store, anchor: anchorFor(dir)}
	_, _, _, hit := cmd.probeFlagCache("prod", "ftp://host", "c-1", "", "", ui.Discard())
	assert.False(t, hit, "unsupported scheme --server must not produce a fast-path cache hit")
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
			// with CodeAuth - the point is the server was reached.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"localProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	// addr is "127.0.0.1:PORT" - the schemeless form of the TLS server's address.
	addr := srv.Listener.Addr().String()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	// Minimal valid config: readable, no current context, no stored credential.
	cfg := &config.Config{}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
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

	// addr is "[::1]:PORT" - the schemeless form of the TLS server's address.
	addr := srv.Listener.Addr().String()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{
			s: map[string]string{"server": addr, "name": "prod", "cluster": "c-1", "cacert": serverCAPEM(srv)},
		},
		ui.Discard())
	require.Error(t, err)
	assert.Greater(t, hits.Load(), int32(0),
		"normalised https://[::1]:port must reach the connection path, not error on a schemeless IPv6 URL")
}

// TestGetTokenFTPServerReadableConfigRejectsResolvedProbe asserts the
// post-resolution probe: a --server ftp://host flag bypasses the fast-path
// validation (which already rejects unsupported schemes), so with a readable
// config the resolved probe must catch it. The test uses a readable (empty)
// config so resolveConn succeeds and the second validation point is exercised,
// proving the seeded cache record is never served.
func TestGetTokenFTPServerReadableConfigRejectsResolvedProbe(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	ftpID := config.CanonicalServerIdentity("ftp://host")
	key := config.CacheKey("prod", ftpID, "c-1")
	require.NoError(t, seedCache(t, dir, store, key, "should-not-be-served", time.Now().Add(time.Hour), "prod", ftpID, "c-1"))
	// Readable (empty) config: resolveConn succeeds, proving the resolved probe is reached.

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "ftp://host", "cluster": "c-1", "name": "prod"}},
		ui.Discard())
	require.Error(t, err, "ftp:// server must be rejected at the resolved-probe validation, not served from cache")
	assert.ErrorContains(t, err, "ftp://host", "error must name the invalid resolved server")
}

// TestGetTokenMalformedServerReadableConfigRejectsResolvedProbe asserts the
// post-resolution probe: a malformed --server https://[::1 (unclosed bracket)
// bypasses the fast-path (CanonicalServerURL errors → fast path skipped), and
// with a readable config the resolved probe must catch it. The seeded cache
// record under the lowercased-fallback identity must not be served.
func TestGetTokenMalformedServerReadableConfigRejectsResolvedProbe(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	malformed := "https://[::1"
	fallbackID := config.CanonicalServerIdentity(malformed)
	key := config.CacheKey("prod", fallbackID, "c-1")
	require.NoError(t, seedCache(t, dir, store, key, "should-not-be-served", time.Now().Add(time.Hour), "prod", fallbackID, "c-1"))
	// Readable (empty) config: resolveConn succeeds, proving the resolved probe is reached.

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
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
	require.NoError(t, seedCache(t, dir, store, key, "should-not-be-served", time.Now().Add(time.Hour), "prod", id, "c-1"))
	// Readable (empty) config.

	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
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
	tok, err := basicAuth(t.Context(), srv.Client(), input, p)
	require.NoError(t, err)

	assert.Contains(t, errBuf.String(), "[alice]", "prompt label must include the default in brackets")
	assert.Equal(t, "alice", loginUsername(t, gotBody), "empty answer must fall back to the default")
	assert.Equal(t, "test-tok:s", tok.Bearer)
}

// TestBasicAuthTrimsPaddedUsername asserts that a
// non-blank answer with incidental padding (a paste with a trailing newline,
// an autocomplete artifact) must be trimmed before it is sent - previously
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
	_, err := basicAuth(t.Context(), srv.Client(), input, p)
	require.NoError(t, err)
	assert.Equal(t, "bob", loginUsername(t, gotBody), "padding must be trimmed from the sent username")
}

// TestCaForServerDeterministicWithMultipleMatches asserts that
// when multiple cfg.Servers entries share a host, caForServer must
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
			got, _ := caForServer(cfg, "https://shared.example", "", false)
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
		got, _ := caForServer(cfg, "https://shared.example", "", false)
		assert.Equal(t, caZeta, got, "current server zeta must win over sorted alpha")
	})

	t.Run("ignoreCurrentContext=true ignores current-server preference", func(t *testing.T) {
		t.Parallel()
		// Same config as "current server wins" but with ignoreCurrentContext=true:
		// even though the current server is "zeta", sorted order (alpha) must win.
		cfg := &config.Config{
			CurrentServer: "zeta",
			Servers: map[string]config.Server{
				"alpha": {URL: "https://shared.example", CA: caAlpha},
				"zeta":  {URL: "https://shared.example", CA: caZeta},
			},
		}
		got, _ := caForServer(cfg, "https://shared.example", "", true)
		assert.Equal(t, caAlpha, got, "ignoreCurrentContext=true: current server zeta must be ignored; sorted alpha wins")
	})
}

// TestResolveConnReadsNoCredential asserts that resolving the connection
// reads no stored credential, whichever of the server, cluster and CA the
// flags leave to the config: errGetTokenStore errors on GetToken for
// "unrelated", the current server, so a call that read its credential would
// return that error. The cases below leave nothing to the config or leave
// it the server and CA.
func TestResolveConnReadsNoCredential(t *testing.T) {
	const inlinePEM = "-----BEGIN CERTIFICATE-----\ndGVzdA==\n-----END CERTIFICATE-----"

	cases := []struct {
		name                 string
		server               string
		cluster              string
		caIn                 string
		ignoreCurrentContext bool
	}{
		{
			// All three flags explicit, ignoreCurrentContext=false:
			// server!="" (1 false), cluster!="" (2 false), caIn!="" (3 false).
			name:   "all explicit flags ignoreCurrentContext false",
			server: "https://test.example.com", cluster: "c-1", caIn: inlinePEM,
			ignoreCurrentContext: false,
		},
		{
			// All three flags explicit, ignoreCurrentContext=true:
			// same logic; ignoreCurrentContext irrelevant when cluster is set.
			name:   "all explicit flags ignoreCurrentContext true",
			server: "https://test.example.com", cluster: "c-1", caIn: inlinePEM,
			ignoreCurrentContext: true,
		},
		{
			// Nothing on the flags: the server and CA come from the current
			// entry, whose login the call does not need.
			name:   "server and CA from the current entry",
			server: "", cluster: "c-1", caIn: "",
			ignoreCurrentContext: false,
		},
		{
			// Shim's common case: explicit server + ignoreCurrentContext=true, caIn="".
			// Config is loaded (caIn==""), but no credential is read.
			name:   "ignoreCurrentContext true empty cluster and caIn",
			server: "https://test.example.com", cluster: "", caIn: "",
			ignoreCurrentContext: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Write a config with a current server pointing at "unrelated".
			// If a credential is read, GetToken for "unrelated" errors → resolveConn
			// propagates the error and the test catches it.
			cfg := &config.Config{
				CurrentServer: "unrelated",
				Servers:       map[string]config.Server{"unrelated": {URL: "https://unrelated.example.com"}},
			}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())

			base := config.NewFileStore(dir)
			store := &errGetTokenStore{CredStore: base, errName: "unrelated"}
			cmd := tokenCmd{dir: dir, store: store}

			_, _, _, err := cmd.resolveConn(tc.server, tc.cluster, tc.caIn, tc.ignoreCurrentContext)
			require.NoError(t, err,
				"resolveConn must read no stored credential (case %q)", tc.name)
		})
	}
}

// TestResolveConnIgnoreCurrentContextRequiresServer asserts the resolveConn side of the
// blank---server class: a ignoreCurrentContext caller with no server must get the
// "--server is required" validation error without any credential being read -
// never the stored current-context URL. errGetTokenStore errors on GetToken
// for "unrelated", so an unexpected credential read would surface as a
// different error and fail the assertions below.
func TestResolveConnIgnoreCurrentContextRequiresServer(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "unrelated",
		Servers:       map[string]config.Server{"unrelated": {URL: "https://unrelated.example.com"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	base := config.NewFileStore(dir)
	store := &errGetTokenStore{CredStore: base, errName: "unrelated"}
	cmd := tokenCmd{dir: dir, store: store}

	_, _, _, err := cmd.resolveConn("", "", "", true)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.ErrorContains(t, ce, "--server is required")
}

// errDeleteCacheStore wraps a CredStore but fails DeleteCachedCredsIfUnchanged
// with a fixed error, for asserting delete-token's error classification
// pass-through.
type errDeleteCacheStore struct {
	config.CredStore
	delErr error
}

func (s *errDeleteCacheStore) DeleteCachedCredsIfUnchanged(map[string]string) ([]string, []string, error) {
	return nil, nil, s.delErr
}

// TestDeleteTokenNamedPreservesLockTimeout asserts that
// a named delete whose store delete times out on the
// token-cache lock must surface LOCK_TIMEOUT (exit 8, retryable), matching
// the --all path's pass-through, not the GENERIC catch-all, and names the
// command to run again.
func TestDeleteTokenNamedPreservesLockTimeout(t *testing.T) {
	dir := t.TempDir()
	base := config.NewFileStore(dir)
	require.NoError(t, seedCache(t, dir, base, "acme/u/c", "tok", time.Now().Add(time.Hour), "", "", ""))
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
	assert.Contains(t, ce.Message, "; run "+clierr.Rancher+" auth delete-token 'acme/u/c' again")
}

// TestCreateDerivedTokenSplits401And403 asserts the two rejections are
// distinct sentinels: 401 is the credential itself, 403 is permission.
func TestCreateDerivedTokenSplits401And403(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status int
		want   error
	}{{http.StatusUnauthorized, errCredentialUnauthorized}, {http.StatusForbidden, errCredentialForbidden}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }))
		_, err := CreateDerivedToken(t.Context(), srv.Client(), srv.URL, "b", "c-test", 0, "d")
		require.ErrorIs(t, err, tc.want)
		srv.Close()
	}
}

// TestResolveClusterIDForCreateClassifiesRejections asserts the steve
// read's 401 is the sentinel the create uses, so the sign-in rule holds; a
// 403 is the collection refusal's RBAC error, never the create's "cannot
// create tokens" sentinel; and every other non-200 stays a generic error.
// Each also holds the status for the steve readers.
func TestResolveClusterIDForCreateClassifiesRejections(t *testing.T) {
	t.Parallel()
	answer := func(status int) (string, *http.Client) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		t.Cleanup(srv.Close)
		return srv.URL, srv.Client()
	}

	url, client := answer(http.StatusUnauthorized)
	_, _, err := resolveClusterIDForCreate(t.Context(), client, url, "b", "testcluster")
	require.ErrorIs(t, err, errCredentialUnauthorized)
	se, ok := clierr.HTTPStatus(err)
	require.True(t, ok)
	assert.Equal(t, http.StatusUnauthorized, se)

	url, client = answer(http.StatusForbidden)
	_, _, err = resolveClusterIDForCreate(t.Context(), client, url, "b", "testcluster")
	require.NotErrorIs(t, err, errCredentialForbidden)
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, "the server refused GET /v1/management.cattle.io.clusters (403); Rancher answers this request with 200 for every signed-in user; a proxy in front of Rancher or an unsupported Rancher refused it", ce.Message)

	url, client = answer(http.StatusBadGateway)
	_, _, err = resolveClusterIDForCreate(t.Context(), client, url, "b", "testcluster")
	ce, ok = errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	require.NotErrorIs(t, err, errCredentialUnauthorized)
	se, ok = clierr.HTTPStatus(err)
	require.True(t, ok)
	assert.Equal(t, http.StatusBadGateway, se)
}

// TestPluginGetterErrors asserts every non-2xx answer holds the status
// (clierr.HTTPStatus): a 401 also matches errCredentialUnauthorized, any
// other status is classified by the status table under the "resolving
// --cluster" prefix (a 5xx is retryable), and a transport failure holds no
// status.
func TestPluginGetterErrors(t *testing.T) {
	t.Parallel()
	rows := []struct {
		status    int
		code      string
		message   string
		retryable bool
	}{
		{http.StatusForbidden, clierr.CodeRBAC, "resolving --cluster: the server refused the request (403) and gave no reason", false},
		{http.StatusNotFound, clierr.CodeNotFound, "resolving --cluster: the server could not find the requested resource", false},
		{http.StatusInternalServerError, clierr.CodeGeneric, `resolving --cluster: an error on the server ("Internal Server Error") has prevented the request from succeeding - retrying may succeed`, true},
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		_, err := pluginGetter(srv.Client(), srv.URL, "b")(t.Context(), steve.Request{Type: steve.ClustersType})
		srv.Close()
		se, ok := clierr.HTTPStatus(err)
		require.True(t, ok, "status %d", status)
		assert.Equal(t, status, se)
		if status == http.StatusUnauthorized {
			require.ErrorIs(t, err, errCredentialUnauthorized)
			continue
		}
		require.NotErrorIs(t, err, errCredentialUnauthorized)
		require.NotErrorIs(t, err, errCredentialForbidden)
		for _, row := range rows {
			if row.status != status {
				continue
			}
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "status %d", status)
			assert.Equal(t, row.code, ce.Code, "status %d", status)
			assert.Equal(t, row.message, ce.Message, "status %d", status)
			assert.Equal(t, row.retryable, ce.Retryable, "status %d", status)
		}
	}

	var auth, accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, accept = r.Header.Get("Authorization"), r.Header.Get("Accept")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	body, err := pluginGetter(srv.Client(), srv.URL, "b")(t.Context(), steve.Request{Type: steve.ClustersType})
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(body))
	assert.Equal(t, "Bearer b", auth)
	assert.Equal(t, "application/json", accept)

	srv.Close()
	_, err = pluginGetter(srv.Client(), srv.URL, "b")(t.Context(), steve.Request{Type: steve.ClustersType})
	require.ErrorContains(t, err, "resolving --cluster: ")
	_, ok := clierr.HTTPStatus(err)
	assert.False(t, ok, "a transport failure has no status")
}

// TestPluginClusterTextListsAtMostTen asserts the plugin NOT_FOUND names
// the clusters the user can read, ten at most, then how many more, and no
// command.
func TestPluginClusterTextListsAtMostTen(t *testing.T) {
	t.Parallel()
	list := []steve.Cluster{{ID: "c-abc", DisplayName: "prod"}, {ID: "c-def"}}
	_, _, err := steve.MatchCluster(list, "web", pluginClusterText("web", list))
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, "cluster not found: 'web'; it is not among the clusters you can read; pass a cluster id or display name; the clusters you can read: 'c-abc' (prod), 'c-def'", ce.Message)
	assert.Empty(t, ce.Hint)
	assert.NotContains(t, ce.Message, clierr.Rancher)

	list = nil
	for i := range 12 {
		list = append(list, steve.Cluster{ID: fmt.Sprintf("c-%02d", i)})
	}
	text := pluginClusterText("web", list)
	assert.True(t, strings.HasSuffix(text.NotFound, "'c-08', 'c-09', and 2 more"), text.NotFound)
	assert.NotContains(t, text.NotFound, "'c-10'")
}

// steveClustersPath is the steve collection of the clusters a bearer can
// read, the plugin's cluster lookup.
const steveClustersPath = "/v1/management.cattle.io.clusters"

// steveClustersBody is one page of the steve clusters collection holding
// the given id and display name pairs.
func steveClustersBody(pairs ...[2]string) []byte {
	items := make([]map[string]any, 0, len(pairs))
	for _, p := range pairs {
		items = append(items, map[string]any{"id": p[0], "type": "management.cattle.io.cluster",
			"metadata": map[string]any{"name": p[0]}, "spec": map[string]any{"displayName": p[1]}})
	}
	page := map[string]any{"revision": "1", "data": items}
	if len(items) > 0 {
		page["count"] = len(items)
	}
	b, _ := json.Marshal(page)
	return b
}

// facadeClusters is the steve clusters collection for a bearer the facade
// recognizes, unknown (401) otherwise, so resolveClusterIDForCreate can be
// exercised against the facade in this package.
func facadeClusters(valid func(string) bool) func(string) ([]testrancher.ClusterFixture, bool) {
	return func(bearer string) ([]testrancher.ClusterFixture, bool) {
		if !valid(bearer) {
			return nil, false
		}
		return []testrancher.ClusterFixture{{ID: "c-test", DisplayName: "testcluster"}}, true
	}
}

// newTokenFacade builds a facade with the steve clusters collection wired
// to its own ValidBearer.
func newTokenFacade(t *testing.T, opts testrancher.Options) (*testrancher.Facade, *httptest.Server) {
	t.Helper()
	f := newFacadeWithClusters(&opts)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

// newInMemoryTokenFacade is newTokenFacade on the in-memory network, for a
// test inside a synctest bubble: the server answers for rancherOrigin
// through the returned transport, never through a listener.
func newInMemoryTokenFacade(t *testing.T, opts testrancher.Options) (*testrancher.Facade, hostTransport) {
	t.Helper()
	f := newFacadeWithClusters(&opts)
	srv := httptest.NewTestServer(t, f)
	return f, hostTransport{hostOf(rancherOrigin): srv.Client().Transport}
}

// newFacadeWithClusters builds the facade with the steve clusters
// collection, which validates bearers against the facade itself.
func newFacadeWithClusters(opts *testrancher.Options) *testrancher.Facade {
	var f *testrancher.Facade
	if opts.ClusterVisibility == nil {
		opts.ClusterVisibility = facadeClusters(func(b string) bool { return f != nil && f.ValidBearer(b) })
	}
	f = testrancher.New(*opts)
	return f
}

// seedEntry writes one server entry and its stored token.
func seedEntry(t *testing.T, dir string, store config.CredStore, name string, s config.Server, tok string, reuse bool) {
	t.Helper()
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	cfg.Servers[name] = s
	cfg.TokenReuse = reuse
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	if tok != "" {
		require.NoError(t, store.SetToken(config.ServerRef{Name: name, URL: s.URL}, tok))
	}
}

// TestStoredTokenForServerRanksUserEntriesAboveExecLogin asserts the
// provenance tier for a call that names no entry: a user entry wins over a
// plugin login entry even when the plugin entry is the current server; with
// the setting off the plugin entry is not a candidate at all. A call that
// names the plugin entry means it, and reads its credential while the
// setting is on.
func TestStoredTokenForServerRanksUserEntriesAboveExecLogin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	plugin := config.Server{URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin, TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "p"}}
	user := config.Server{URL: "https://r.ex"}
	cfg := &config.Config{Servers: map[string]config.Server{"exec-login-r.ex": plugin, "zz-user": user}, CurrentServer: "exec-login-r.ex", TokenReuse: true}
	require.NoError(t, store.SetToken(config.ServerRef{Name: "exec-login-r.ex", URL: "https://r.ex"}, "ext/p:s"))
	require.NoError(t, store.SetToken(config.ServerRef{Name: "zz-user", URL: "https://r.ex"}, "ext/u:s"))

	got, ok, err := lookupStored(cfg, store, "https://r.ex", "", "", false)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "zz-user", got.name, "a user entry outranks the plugin entry, current server or not")
	assert.Empty(t, got.entry.Provenance)

	got, ok, err = lookupStored(cfg, store, "https://r.ex", "exec-login-r.ex", "", false)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "exec-login-r.ex", got.name, "naming the plugin entry means it")

	got, ok, err = lookupStored(cfg, store, "https://r.ex", "", "", true)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "zz-user", got.name, "the shim's ordering keeps user before plugin too")

	require.NoError(t, store.DeleteToken(config.ServerRef{Name: "zz-user", URL: "https://r.ex"}))
	got, ok, err = lookupStored(cfg, store, "https://r.ex", "", "", false)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "exec-login-r.ex", got.name)
	assert.Equal(t, clierr.ProvenanceExecLogin, got.entry.Provenance)

	cfg.TokenReuse = false
	_, ok, err = lookupStored(cfg, store, "https://r.ex", "", "", false)
	require.NoError(t, err)
	assert.False(t, ok, "with the setting off a plugin login entry is skipped entirely")
	_, ok, err = lookupStored(cfg, store, "https://r.ex", "exec-login-r.ex", "", false)
	require.NoError(t, err)
	assert.False(t, ok, "and naming it does not read it")
}

// TestStoredCredTokenClassifiesRejections asserts the 401/403 rules per
// provenance and per request: a user credential's 401 (cluster list or
// create) is the scoped-interactive sentinel with the auth login notice;
// a plugin credential's 401 is the refresh sentinel naming the entry; a
// 403 from either request is an RBAC error for both provenances.
func TestStoredCredTokenClassifiesRejections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		provenance clierr.Provenance
		opts       testrancher.Options
		stale      bool // the stored bearer is not a facade token
		cluster    string
		want       string // "rejected", "plugin", "rbac"
	}{
		{"user 401 on create", "", testrancher.Options{}, true, "local", "rejected"},
		{"user 401 on cluster list", "", testrancher.Options{}, true, "testcluster", "rejected"},
		{"plugin 401 on create", clierr.ProvenanceExecLogin, testrancher.Options{}, true, "local", "plugin"},
		{"plugin 401 on cluster list", clierr.ProvenanceExecLogin, testrancher.Options{}, true, "testcluster", "plugin"},
		{"user 403 on create", "", testrancher.Options{ScopedCreateStatus: http.StatusForbidden}, false, "local", "rbac"},
		{"plugin 403 on create", clierr.ProvenanceExecLogin, testrancher.Options{ScopedCreateStatus: http.StatusForbidden}, false, "local", "rbac"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, srv := newTokenFacade(t, tc.opts)
			dir := t.TempDir()
			store := config.NewFileStore(dir)
			bearer := "ext/stale:s"
			if !tc.stale {
				cred, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{Server: srv.URL, AuthProvider: "localProvider", ResponseType: "json"}, 0,
					ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true))
				require.NoError(t, err)
				bearer = cred.Token
			}
			seedEntry(t, dir, store, "e", config.Server{URL: srv.URL, Provenance: tc.provenance}, bearer, true)
			base := f.LoginCount()
			var notices bytes.Buffer
			p := ui.New(&notices, newLineReader(), true, false)
			_, _, _, err := tokenCmd{dir: dir, store: store}.storedCredToken(t.Context(), srv.Client(), srv.URL, tc.cluster, "e", false, p)
			switch tc.want {
			case "rejected":
				require.ErrorIs(t, err, errStoredCredentialRejected)
				assert.Contains(t, notices.String(), "rancher auth login")
			case "plugin":
				rej, ok := errors.AsType[*pluginCredentialRejected](err)
				require.True(t, ok, "%v", err)
				assert.Equal(t, "e", rej.name)
			case "rbac":
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "%v", err)
				assert.Equal(t, clierr.CodeRBAC, ce.Code)
				assert.Contains(t, ce.Message, "403")
			}
			assert.Equal(t, base, f.LoginCount(), "classification never starts a login")
		})
	}
}

// TestStoredCredTokenPluginLaneFailsClosed asserts the plugin-created
// login's silent path: the ext create's scope is validated, a Rancher
// without ext.cattle.io uses the v3 create, and when neither works the
// unscoped credential is never served; the user lane keeps its legacy
// fallback unchanged beside it.
func TestStoredCredTokenPluginLaneFailsClosed(t *testing.T) {
	t.Parallel()
	other := "c-other"
	cases := []struct {
		name       string
		provenance clierr.Provenance
		opts       testrancher.Options
		wantErr    error
		wantServed string // "cluster", "stored", or ""
	}{
		{"plugin scope mismatch", clierr.ProvenanceExecLogin, testrancher.Options{ScopedCreateClusterName: &other}, errClusterTokenUnscoped, ""},
		{"user scope mismatch unchanged", "", testrancher.Options{ScopedCreateClusterName: &other}, nil, "cluster"},
		{"plugin legacy via v3", clierr.ProvenanceExecLogin, testrancher.Options{Legacy: true, V3TokenCreate: true}, nil, "cluster"},
		{"plugin legacy too old", clierr.ProvenanceExecLogin, testrancher.Options{Legacy: true}, errServerTooOldForReuse, ""},
		{"user legacy serves stored", "", testrancher.Options{Legacy: true}, nil, "stored"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, srv := newTokenFacade(t, tc.opts)
			cred, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{Server: srv.URL, AuthProvider: "localProvider", ResponseType: "json"}, 0,
				ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true))
			require.NoError(t, err)
			dir := t.TempDir()
			store := config.NewFileStore(dir)
			seedEntry(t, dir, store, "e", config.Server{URL: srv.URL, Provenance: tc.provenance}, cred.Token, true)
			tok, _, _, err := tokenCmd{dir: dir, store: store}.storedCredToken(t.Context(), srv.Client(), srv.URL, "c-test", "e", false, ui.Discard())
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				assert.Empty(t, tok.Token)
				return
			}
			require.NoError(t, err)
			switch tc.wantServed {
			case "stored":
				assert.Equal(t, cred.Token, tok.Token)
			case "cluster":
				assert.NotEqual(t, cred.Token, tok.Token)
			}
		})
	}
}

// TestStoredCredTokenMetadataNeverDrivesServing asserts the metadata is
// display-only: an expired-looking credentialExpiresAt never blocks a
// credential the server still accepts, and a live-looking one never
// rescues a credential the server rejects.
func TestStoredCredTokenMetadataNeverDrivesServing(t *testing.T) {
	t.Parallel()
	_, srv := newTokenFacade(t, testrancher.Options{})
	cred, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{Server: srv.URL, AuthProvider: "localProvider", ResponseType: "json"}, 0,
		ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true))
	require.NoError(t, err)

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	seedEntry(t, dir, store, "e", config.Server{URL: srv.URL, Provenance: clierr.ProvenanceDerived, TokenRef: cred.Ref, CredentialExpiresAt: "2000-01-01T00:00:00Z"}, cred.Token, true)
	tok, _, _, err := tokenCmd{dir: dir, store: store}.storedCredToken(t.Context(), srv.Client(), srv.URL, "c-test", "e", false, ui.Discard())
	require.NoError(t, err, "expired-looking metadata never blocks a valid credential")
	assert.NotEmpty(t, tok.Token)

	dir2 := t.TempDir()
	store2 := config.NewFileStore(dir2)
	seedEntry(t, dir2, store2, "e", config.Server{URL: srv.URL, Provenance: clierr.ProvenanceDerived, TokenRef: cred.Ref, CredentialExpiresAt: "2999-01-01T00:00:00Z"}, "ext/stale:s", true)
	_, _, _, err = tokenCmd{dir: dir2, store: store2}.storedCredToken(t.Context(), srv.Client(), srv.URL, "c-test", "e", false, ui.Discard())
	require.ErrorIs(t, err, errStoredCredentialRejected, "live-looking metadata never rescues a rejected credential")
}

// TestGetTokenUser401FallsBackToScopedInteractiveNeverPluginLogin asserts
// the Trigger rule with the setting on: a stale user credential runs
// today's scoped interactive login and creates no plugin login entry.
func TestGetTokenUser401FallsBackToScopedInteractiveNeverPluginLogin(t *testing.T) {
	t.Parallel()
	f, srv := newTokenFacade(t, testrancher.Options{})
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	seedEntry(t, dir, store, "prod", config.Server{URL: srv.URL}, "ext/stale:s", true)
	cmd := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": srv.URL, "cluster": "c-test", "name": "prod", "auth-provider": "localProvider"}},
		ui.New(&bytes.Buffer{}, newLineReader("e2e-user", "e2e-pass"), false, true))
	require.NoError(t, err)
	assert.Equal(t, 1, f.LoginCount())
	descs := f.LoginDescriptions()
	require.Len(t, descs, 2, "the login token and the cluster token carry the description")
	assert.Equal(t, descs[0], descs[1])
	assert.Contains(t, descs[0], "[op:", "the sign-in's tokens are marked")
	assert.Equal(t, "ExecCredential", res.Kind)
	cfg, _ := config.Load(config.FilePath(dir))
	for name := range cfg.Servers {
		assert.False(t, config.IsExecLoginEntryName(name), "no plugin login entry appears beside a stale user credential")
	}
	_, err = store.GetToken(config.ServerRef{Name: config.ExecLoginEntryName(srv.URL), URL: srv.URL})
	require.ErrorIs(t, err, config.ErrNoToken)
}

// TestGetTokenCreateForbiddenIsRBACNotInteractive asserts the one behavior
// change to the existing lane: a 403 on the create surfaces the permission
// error and never re-prompts, since a new login yields the same RBAC.
func TestGetTokenCreateForbiddenIsRBACNotInteractive(t *testing.T) {
	var authProviderHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1-public/authproviders"):
			authProviderHits.Add(1)
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == ExtTokensPath && r.Method == http.MethodPost:
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
	_, err := newTokenCommandForTest(t, dir, store).(runtime.ReadCommand).Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}}, ui.Discard())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok)
	assert.Equal(t, clierr.CodeRBAC, ce.Code)
	assert.Equal(t, int32(0), authProviderHits.Load(), "a 403 never falls back to the interactive flow")
}

// TestStoredTokenForServerBlankTokenIsNotACredential asserts a stored value
// that is only whitespace is a miss, not a credential: it neither wins nor
// makes a tie with the entry that holds a real token.
func TestStoredTokenForServerBlankTokenIsNotACredential(t *testing.T) {
	store := config.NewFileStore(t.TempDir())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod-a"}, "  \n"))
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod-b"}, "tok-b"))
	cfg := &config.Config{Servers: map[string]config.Server{
		"prod-a": {URL: "https://r.ex"},
		"prod-b": {URL: "https://r.ex"},
	}}

	got, ok, serr := lookupStored(cfg, store, "https://r.ex", "", "", false)
	require.NoError(t, serr)
	require.True(t, ok)
	assert.Equal(t, "prod-b", got.name)

	got, ok, serr = lookupStored(cfg, store, "https://r.ex", "prod-a", "", false)
	require.NoError(t, serr)
	assert.False(t, ok, "an identified entry holding only whitespace has no credential: %q", got.token)
}

// createdClusterName returns spec.clusterName of an ext token create
// request body, empty when the field is absent. It may run in a server
// handler, so a body that does not decode fails the test without stopping
// it.
func createdClusterName(t *testing.T, body []byte) string {
	t.Helper()
	var req struct {
		Spec struct {
			ClusterName string `json:"clusterName"`
		} `json:"spec"`
	}
	assert.NoError(t, json.Unmarshal(body, &req), "create request body %s", body)
	return req.Spec.ClusterName
}

// loginUsername returns the username of a login request body.
func loginUsername(t *testing.T, body []byte) string {
	t.Helper()
	var req struct {
		Username string `json:"username"`
	}
	require.NoError(t, json.Unmarshal(body, &req), "login request body %s", body)
	return req.Username
}
