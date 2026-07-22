package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func (t tin) String(k string) string { return t.s[k] }
func (t tin) Bool(k string) bool     { return t.b[k] }
func (t tin) Args() []string         { return t.a }

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
		_, err := cmd.Execute(context.Background(), tin{}, ui.Discard())
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeValidation, ce.Code)
	})

	t.Run("--all flushes the cache", func(t *testing.T) {
		require.NoError(t, store.SetCachedCred("acme/u/c", "tok", time.Now().Add(time.Hour), "", "", ""))
		_, err := cmd.Execute(context.Background(), tin{b: map[string]bool{"all": true}}, ui.Discard())
		require.NoError(t, err)
		_, _, gerr := store.GetCachedCred("acme/u/c")
		assert.ErrorIs(t, gerr, config.ErrNoToken)
	})

	t.Run("named delete removes the entry", func(t *testing.T) {
		require.NoError(t, store.SetCachedCred("acme/u/c", "tok", time.Now().Add(time.Hour), "", "", ""))
		_, err := cmd.Execute(context.Background(), tin{a: []string{"acme/u/c"}}, ui.Discard())
		require.NoError(t, err)
		_, _, gerr := store.GetCachedCred("acme/u/c")
		assert.ErrorIs(t, gerr, config.ErrNoToken)
	})

	t.Run("--all with a name is a validation error, not a silent flush", func(t *testing.T) {
		require.NoError(t, store.SetCachedCred("keep/u/c", "tok", time.Now().Add(time.Hour), "", "", ""))
		_, err := cmd.Execute(context.Background(), tin{b: map[string]bool{"all": true}, a: []string{"keep/u/c"}}, ui.Discard())
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
		res, err := cmd.Execute(context.Background(), tin{a: []string{"prod"}}, ui.Discard())
		require.NoError(t, err)
		deleted := res.Data.(map[string]any)["deleted"].([]string)
		assert.ElementsMatch(t, []string{"prod", scoped}, deleted)
		_, _, gerr := store.GetCachedCred("prod")
		assert.ErrorIs(t, gerr, config.ErrNoToken)
		_, _, gerr = store.GetCachedCred(scoped)
		assert.ErrorIs(t, gerr, config.ErrNoToken)
	})

	t.Run("unknown name is an idempotent success", func(t *testing.T) {
		res, err := cmd.Execute(context.Background(), tin{a: []string{"ghost"}}, ui.Discard())
		require.NoError(t, err)
		deleted := res.Data.(map[string]any)["deleted"].([]string)
		assert.Empty(t, deleted, "already-deleted / unknown names must be a no-op success")
	})
}

func TestBasicAuthNonInteractiveIsAuthError(t *testing.T) {
	t.Parallel()
	input := &loginInput{server: "https://test.example.com", authProvider: "localProvider"}
	_, err := basicAuth(context.Background(), nil, input, ui.Discard())
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
	_, err := cmd.Execute(context.Background(), tin{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestGetTokenCacheHit(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetCachedCred(config.CacheKey("prod", "https://r.ex", ""), "cached-tok", time.Now().Add(time.Hour), "prod", "https://r.ex", ""))
	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)

	res, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": "https://r.ex", "name": "prod"}},
		ui.Discard())
	require.NoError(t, err)
	assert.True(t, res.Raw)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "cached-tok", cred.Status.Token)
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
	res, err := cmd.Execute(context.Background(),
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
	require.NoError(t, store.SetCachedCred(config.CacheKey("prod", "https://r1.example.com", ""), "tok-r1", time.Now().Add(time.Hour), "prod", "https://r1.example.com", ""))
	require.NoError(t, store.SetCachedCred(config.CacheKey("prod", "https://r2.example.com", ""), "tok-r2", time.Now().Add(time.Hour), "prod", "https://r2.example.com", ""))
	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)

	res, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": "https://r2.example.com", "name": "prod"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "tok-r2", cred.Status.Token, "get-token must return the token scoped to the requested server")
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
			_, _ = w.Write([]byte(`{"status":{"bearerToken":"ext/minted:secret"}}`))
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
	res, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod"}},
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
	res, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "stored-login-token", cred.Status.Token)

	_, expiry, cerr := store.GetCachedCred(config.CacheKey("prod", config.CanonicalServerIdentity(srv.URL), ""))
	require.NoError(t, cerr, "the legacy fallback must be cached, not treated as zero-expiry")
	assert.WithinDuration(t, time.Now().Add(legacyMintTTL), expiry, time.Minute)
}

func TestGetTokenFallsBackToInteractiveWhenMintRejected(t *testing.T) {
	var authProviderHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1-public/authproviders"):
			atomic.AddInt32(&authProviderHits, 1)
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
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod"}},
		ui.Discard())
	// SAML under a non-interactive prompter fast-fails AUTH — the point here
	// is that the flow reached the provider dance at all.
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, int32(1), atomic.LoadInt32(&authProviderHits), "a rejected mint must fall through to the interactive flow")
}

// TestGetTokenFallsBackToInteractiveWithNoStoredCredential covers the "no
// stored credential exists" half of the fallback.
func TestGetTokenFallsBackToInteractiveWithNoStoredCredential(t *testing.T) {
	var authProviderHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			atomic.AddInt32(&authProviderHits, 1)
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
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, int32(1), atomic.LoadInt32(&authProviderHits), "no stored credential must fall through to the interactive flow")
}

// TestGetTokenCachesMintedExpiry is the round-2 repro for finding 1: the
// stored-credential mint's status.expiresAt must be propagated into the
// cached credential's real expiry, not silently dropped.
func TestGetTokenCachesMintedExpiry(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == MintTokensPath && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"status":{"bearerToken":"ext/minted:secret","expiresAt":%q}}`, expiresAt)
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
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod"}},
		ui.Discard())
	require.NoError(t, err)

	_, gotExpiry, cerr := store.GetCachedCred(config.CacheKey("prod", config.CanonicalServerIdentity(srv.URL), ""))
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
			_, _ = w.Write([]byte(`{"status":{"bearerToken":"ext/minted:secret"}}`))
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
	res, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod"}},
		ui.New(&notices, strings.NewReader(""), true, false))
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "ext/minted:secret", cred.Status.Token, "the token is still usable for this call")

	_, _, cerr := store.GetCachedCred(config.CacheKey("prod", config.CanonicalServerIdentity(srv.URL), ""))
	assert.ErrorIs(t, cerr, config.ErrNoToken, "a zero-expiry token must not be cached")
	assert.Contains(t, notices.String(), "not caching")
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
			_, _ = w.Write([]byte(`{"status":{"bearerToken":"ext/minted:secret"}}`))
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
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "cluster": "c-a"}},
		ui.Discard())
	require.NoError(t, err)
	_, err = cmd.Execute(context.Background(),
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
	var listHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == clustersPath:
			atomic.AddInt32(&listHits, 1)
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"c-m-abc"},"spec":{"displayName":"prod"}}]}`))
		case r.URL.Path == MintTokensPath && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			gotCluster = gjson.GetBytes(body, "spec.clusterName").String()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":{"bearerToken":"ext/minted:secret"}}`))
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
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "cluster": "prod"}},
		ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "c-m-abc", gotCluster, "the resolved id, not the raw display name, must reach the mint")
	assert.Equal(t, int32(1), atomic.LoadInt32(&listHits))
	assert.Equal(t, "Bearer stored-login-token", gotAuth, "cluster resolution uses the stored credential, not a fresh session")
}

// TestGetTokenClusterMissErrorsWithoutMinting is the round-3 repro for
// finding 3's "unresolvable → propagate the resolution error" half: an
// unresolvable --cluster must fail directly, never mint an unscoped token.
func TestGetTokenClusterMissErrorsWithoutMinting(t *testing.T) {
	var mintHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case clustersPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[]}`))
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
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "cluster": "missing"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, int32(0), atomic.LoadInt32(&mintHits), "an unresolvable cluster must never reach the mint call")
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
			// display name.
			_, _ = w.Write([]byte(`{"items":[
				{"metadata":{"name":"c-1"},"spec":{"displayName":"prod"}},
				{"metadata":{"name":"c-2"},"spec":{"displayName":"c-1"}}
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
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "cluster": "c-1"}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.ElementsMatch(t, []string{"c-1", "c-2"}, candidateIDsToken(ce.Candidates))
	assert.Equal(t, int32(1), atomic.LoadInt32(&listHits), "no list scan beyond the collision check")
	assert.Equal(t, int32(0), atomic.LoadInt32(&mintHits), "an ambiguous cluster must never reach the mint call")
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
	name, tok, _, ok, serr := storedTokenForServer(cfg, store, "https://r.ex", "prod-a", ui.New(&notices, strings.NewReader(""), true, false))
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
	var authProviderHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1-public/authproviders"):
			atomic.AddInt32(&authProviderHits, 1)
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
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod"}},
		ui.Discard())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "interactive login required")
	assert.Contains(t, err.Error(), "500")
	assert.Equal(t, int32(0), atomic.LoadInt32(&authProviderHits), "a real mint failure must not fall through to the interactive flow")
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
	// --cluster and a (garbage but unparsed, since --skip-verify bypasses
	// CA loading) --cacert are set explicitly so resolveConn's OWN
	// config.yaml read is skipped (all three of server/cluster/caIn are
	// non-empty) — the point of this test is storedCredToken's separate,
	// unconditional config.Load call.
	_, err := cmd.Execute(context.Background(),
		tin{
			s: map[string]string{
				"server": "https://r.ex", "cluster": "c-1",
				"cacert": "-----BEGIN CERTIFICATE-----\nMIICertData\n-----END CERTIFICATE-----\n",
				"name":   "prod",
			},
			b: map[string]bool{"skip-verify": true},
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
		name, tok, _, ok, serr := storedTokenForServer(cfg, store, "https://r.ex", "", ui.Discard())
		require.NoError(t, serr)
		require.True(t, ok)
		assert.Equal(t, "prod-a", name)
		assert.Equal(t, "tok-a", tok)
	})

	t.Run("the current context's server wins the tie, regardless of name order", func(t *testing.T) {
		cfg.CurrentContext = "ctx-b"
		cfg.Contexts = map[string]config.Context{"ctx-b": {Server: "prod-b"}}
		name, tok, _, ok, serr := storedTokenForServer(cfg, store, "https://r.ex", "", ui.Discard())
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
	name, tok, _, ok, serr := storedTokenForServer(cfg, store, "https://r.ex", "", ui.Discard())
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
	name, tok, _, ok, serr := storedTokenForServer(cfg, store, "https://r.ex", "prod-b", ui.Discard())
	require.NoError(t, serr)
	require.True(t, ok)
	assert.Equal(t, "prod-b", name, "--name must win over the lexicographic default")
	assert.Equal(t, "tok-b", tok)
}

// TestStoredTokenForServerMatchesEquivalentURLSpellings is the impl-review
// finding-4 repro: candidate matching must use canonical server identity, so
// a configured "https://host" is found for a "HTTPS://HOST:443" query (case
// and an explicit default port are irrelevant) rather than skipped.
func TestStoredTokenForServerMatchesEquivalentURLSpellings(t *testing.T) {
	store := config.NewFileStore(t.TempDir())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "tok"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: "https://host"}}}

	name, tok, _, ok, serr := storedTokenForServer(cfg, store, "HTTPS://HOST:443", "", ui.Discard())
	require.NoError(t, serr)
	require.True(t, ok, "an equivalent URL spelling must still match the configured server")
	assert.Equal(t, "prod", name)
	assert.Equal(t, "tok", tok)
}

func TestSelectAuthProviderExplicitMatch(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&LocalProvider{AuthProvider{Type: "localProvider"}}, &AuthProvider{Type: "pingProvider"}}
	got, err := selectAuthProvider(context.Background(), ps, "pingProvider", ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "pingProvider", got.GetType())
}

func TestSelectAuthProviderNotFound(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&AuthProvider{Type: "pingProvider"}}
	_, err := selectAuthProvider(context.Background(), ps, "nope", ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Contains(t, candidateIDs(ce.Candidates), "pingProvider")
}

func TestSelectAuthProviderSingle(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&AuthProvider{Type: "pingProvider"}}
	got, err := selectAuthProvider(context.Background(), ps, "", ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "pingProvider", got.GetType())
}

func TestSelectAuthProviderAmbiguousNoInput(t *testing.T) {
	t.Parallel()
	ps := []typedProvider{&AuthProvider{Type: "pingProvider"}, &AuthProvider{Type: "oktaProvider"}}
	_, err := selectAuthProvider(context.Background(), ps, "", ui.Discard())
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
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod"}},
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
		CurrentContext: "acme",
		Contexts:       map[string]config.Context{"acme": {Server: "acme", Cluster: "local"}},
		Servers: map[string]config.Server{
			"acme":  {URL: "https://config.example", CA: acmeCA},
			"other": {URL: "https://other.example", CA: otherCA},
		},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := tokenCmd{dir: dir, store: store}

	t.Run("no flags sources from config", func(t *testing.T) {
		server, ca, cluster, err := cmd.resolveConn(tin{})
		require.NoError(t, err)
		assert.Equal(t, "https://config.example", server)
		assert.Equal(t, "local", cluster)
		assert.Equal(t, acmeCA, ca, "inline PEM passes through resolveCA")
	})

	t.Run("explicit --server matching another configured entry uses that entry's CA, not the current context's", func(t *testing.T) {
		server, ca, cluster, err := cmd.resolveConn(tin{s: map[string]string{"server": "https://other.example"}})
		require.NoError(t, err)
		assert.Equal(t, "https://other.example", server)
		assert.Equal(t, "local", cluster, "cluster still from config")
		assert.Equal(t, otherCA, ca, "CA comes from the entry matching the requested server")
	})

	t.Run("explicit --server matching no configured entry gets no custom CA", func(t *testing.T) {
		server, ca, cluster, err := cmd.resolveConn(tin{s: map[string]string{"server": "https://flag.example"}})
		require.NoError(t, err)
		assert.Equal(t, "https://flag.example", server)
		assert.Equal(t, "local", cluster, "cluster still from config")
		assert.Empty(t, ca, "no configured entry matches — never borrow the current context's CA for a different server")
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
	_, err := cmd.Execute(context.Background(), tin{s: map[string]string{"server": srv.URL}}, p)
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
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod"}},
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
	res, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod"}},
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
	require.NoError(t, store.SetCachedCred(config.CacheKey("prod", "https://r.ex", ""), "ext/token-abc:secretxyz", time.Now().Add(time.Hour), "prod", "https://r.ex", ""))
	cmd := NewTokenCommand(dir, store).(runtime.ReadCommand)

	res, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": "https://r.ex", "name": "prod"}},
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
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "auth-flow": "devicecode"}},
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
	_, err := cmd.Execute(context.Background(),
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
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "auth-flow": "authcode"}},
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
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "auth-flow": "authcode"}},
		p)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "interactive login required", "non-TTY must bypass the gate")
	assert.Contains(t, err.Error(), "callback port", "should fail past the gate on the forced port error")
}

func TestOAuthAuthCodeAuthHonorsCallerContext(t *testing.T) {
	// Regression pin for the exchange-ctx/rancherLogin-ctx shadowing bug: the
	// caller's context must still govern the flow after the code exchange,
	// with zero timing sensitivity (no slow-IdP race).
	t.Parallel()

	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fake-access-token","token_type":"bearer","expires_in":3600}`))
	}))
	defer idp.Close()

	var rancherHits int32
	rancherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rancherHits, 1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer rancherSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
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
				AuthURL:  idp.URL + "/authorize",
				TokenURL: idp.URL + "/token",
			},
		},
	}
	input := &loginInput{server: rancherSrv.URL, authProvider: "azureADProvider"}

	_, err := oauthAuthCodeAuth(ctx, http.DefaultClient, input, provider, 5*time.Second, openBrowserStub, ui.Discard())
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "error must wrap context.Canceled, got: %v", err)
	assert.Equal(t, int32(0), atomic.LoadInt32(&rancherHits),
		"Rancher login must never be reached once the caller context is cancelled")
}

func TestOAuthAuthCodeAuthAbortsOnCallerCancelDuringBrowserWait(t *testing.T) {
	// Regression pin: cancellation while still WAITING for the browser
	// callback (no callback ever arrives) must abort promptly via the wait
	// select's ctx.Done() case, not surface later as a confusing exchange
	// failure or wait for the flow timeout.
	t.Parallel()

	var rancherHits int32
	rancherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rancherHits, 1)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer rancherSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
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
				AuthURL:  "http://127.0.0.1:0/authorize", // never reached
				TokenURL: "http://127.0.0.1:0/token",
			},
		},
	}
	input := &loginInput{server: rancherSrv.URL, authProvider: "azureADProvider"}

	_, err := oauthAuthCodeAuth(ctx, http.DefaultClient, input, provider, 5*time.Second, openBrowserStub, ui.Discard())
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "error must wrap context.Canceled, got: %v", err)
	assert.Equal(t, int32(0), atomic.LoadInt32(&rancherHits))
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
				AuthURL:  "http://127.0.0.1:0/authorize", // never reached
				TokenURL: "http://127.0.0.1:0/token",
			},
		},
	}
	input := &loginInput{server: "http://ignored", authProvider: "azureADProvider"}

	_, err := oauthAuthCodeAuth(context.Background(), http.DefaultClient, input, provider, 5*time.Second, openBrowserStub, ui.Discard())
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

	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				AuthURL:  idp.URL + "/authorize",
				TokenURL: idp.URL + "/token",
			},
		},
	}
	input := &loginInput{server: rancherSrv.URL, authProvider: "azureADProvider"}

	var noticeBuf strings.Builder
	p := ui.New(&noticeBuf, strings.NewReader(""), true, false)

	tok, err := oauthAuthCodeAuth(context.Background(), http.DefaultClient, input, provider, 5*time.Second, openBrowserStub, p)
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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // token never ready — keeps polling
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	input := &loginInput{server: srv.URL}

	go func() {
		time.Sleep(50 * time.Millisecond) // well before the first 500ms poll tick
		cancel()
	}()

	start := time.Now()
	_, err := samlAuth(ctx, http.DefaultClient, input, ui.Discard())
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "error must wrap context.Canceled, got: %v", err)
	assert.Less(t, elapsed, 300*time.Millisecond,
		"must abort promptly on ctx cancellation, not wait for the next poll tick (500ms away)")
}

// blockingReader never returns from Read, so readLine's scan goroutine cannot
// win the select against a cancelled context — making the ctrl-C test deterministic.
type blockingReader struct{}

func (blockingReader) Read([]byte) (int, error) { select {} }

func TestSelectAuthProviderCtrlCIsNotAmbiguous(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var errBuf strings.Builder
	// A blocking reader (not an empty reader): an empty reader hits EOF instantly,
	// racing ctx.Done() in the select and intermittently returning io.EOF.
	p := ui.New(&errBuf, blockingReader{}, false, true)
	providers := []typedProvider{&AuthProvider{Type: "pingProvider"}, &AuthProvider{Type: "oktaProvider"}}
	_, err := selectAuthProvider(ctx, providers, "", p)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), "cancelled context must return context.Canceled, not AMBIGUOUS_SELECTION")
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
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod"}},
		p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404", "a 404 from /v1-public/authproviders must surface as an error")
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, paths)
	for _, p := range paths {
		assert.NotContains(t, p, "v3-public", "no request may fall back to the removed /v3-public API")
	}
}
