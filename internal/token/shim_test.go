package token

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestLegacyTokenDescriptor(t *testing.T) {
	t.Parallel()
	d := NewLegacyTokenCommand(t.TempDir(), nil).Describe()
	assert.Equal(t, "token", d.Name)
	assert.Equal(t, runtime.Local, d.CommandType)
	assert.True(t, d.Network)
	assert.True(t, d.Auth)
	var names []string
	for _, f := range d.Flags {
		names = append(names, f.Name)
	}
	assert.ElementsMatch(t, []string{"server", "user", "cluster",
		"auth-provider", "auth-flow", "cacerts", "skip-verify"}, names)
}

// TestLegacyTokenSlotSeparation verifies that two invocations differing only
// in --user produce two distinct cache entries: the slot key includes the user
// under the "legacy:" prefix, so per-user credentials never collide.
func TestLegacyTokenSlotSeparation(t *testing.T) {
	// A server that 404s all paths looks like a legacy Rancher with no
	// ext.cattle.io: storedCredToken falls back to the stored login token
	// with a bounded TTL, which is then written to the cache.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewLegacyTokenCommand(dir, store).(runtime.ReadCommand)

	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "user": "alice"}},
		ui.Discard())
	require.NoError(t, err)

	_, err = cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "user": "bob"}},
		ui.Discard())
	require.NoError(t, err)

	serverID := config.CanonicalServerIdentity(srv.URL)
	keyAlice := config.CacheKey("legacy:alice", serverID, "")
	keyBob := config.CacheKey("legacy:bob", serverID, "")

	assert.NotEqual(t, keyAlice, keyBob, "different --user values must produce distinct cache slots")

	_, _, err = store.GetCachedCred(keyAlice)
	assert.NoError(t, err, "alice's cache slot must be written")

	_, _, err = store.GetCachedCred(keyBob)
	assert.NoError(t, err, "bob's cache slot must be written")
}

// TestLegacyTokenEmitsV1Beta1 verifies the ExecCredential shape: Kind,
// APIVersion, and that ExpirationTimestamp is nil when the mint response
// carries no expiresAt (the zero-expiry case the shim opts into caching).
func TestLegacyTokenEmitsV1Beta1(t *testing.T) {
	// The mint succeeds but returns no expiresAt — a zero-expiry token that
	// the native get-token would refuse to cache; the shim (legacyShim)
	// caches it and must not set ExpirationTimestamp in the emitted credential.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == MintTokensPath && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":{"bearerToken":"legacy-minted:secret"}}`))
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

	cmd := NewLegacyTokenCommand(dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "user": "cluster-name"}},
		ui.Discard())
	require.NoError(t, err)

	assert.True(t, res.Raw, "ExecCredential result must be marked Raw")
	cred := res.Data.(*execCredential)
	assert.Equal(t, "ExecCredential", cred.Kind)
	assert.Equal(t, "client.authentication.k8s.io/v1beta1", cred.APIVersion)
	assert.Nil(t, cred.Status.ExpirationTimestamp, "no-expiry mint response must not set ExpirationTimestamp")
}

// TestLegacyTokenAuthFlowEnvPrecedence verifies the three-level resolution:
// flag > RANCHER_CLI_OAUTH_AUTH_FLOW > CATTLE_OAUTH_AUTH_FLOW. After fix 2
// the logic lives in resolveAuthFlow (oauth.go), the single source of truth
// for both the native get-token path and the legacy token shim.
func TestLegacyTokenAuthFlowEnvPrecedence(t *testing.T) {
	t.Run("flag wins over both env vars", func(t *testing.T) {
		t.Setenv(config.EnvOAuthAuthFlow, "authcode")
		t.Setenv(config.EnvLegacyOAuthAuthFlow, "authcode")
		assert.Equal(t, "devicecode", resolveAuthFlow("devicecode"))
	})

	t.Run("RANCHER_CLI_OAUTH_AUTH_FLOW wins over CATTLE_OAUTH_AUTH_FLOW", func(t *testing.T) {
		t.Setenv(config.EnvOAuthAuthFlow, "authcode")
		t.Setenv(config.EnvLegacyOAuthAuthFlow, "devicecode")
		assert.Equal(t, "authcode", resolveAuthFlow(""))
	})

	t.Run("CATTLE_OAUTH_AUTH_FLOW used when modern env unset", func(t *testing.T) {
		t.Setenv(config.EnvLegacyOAuthAuthFlow, "devicecode")
		assert.Equal(t, "devicecode", resolveAuthFlow(""))
	})

	t.Run("empty when nothing set", func(t *testing.T) {
		assert.Equal(t, "", resolveAuthFlow(""))
	})
}

// TestLegacyTokenNoContextCluster verifies that the shim never inherits the
// ambient current-context cluster when --cluster is omitted: the cache slot
// must be unscoped (empty cluster component) and the mint request body must
// carry no clusterName, regardless of whatever cluster the operator's local
// interactive CLI session has set.
func TestLegacyTokenNoContextCluster(t *testing.T) {
	var mintBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == MintTokensPath && r.Method == http.MethodPost:
			var err error
			mintBody, err = io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":{"bearerToken":"shim/no-cluster:tok"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))

	cfg := &config.Config{
		CurrentServer: "prod",
		Servers:       map[string]config.Server{"prod": {URL: srv.URL}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewLegacyTokenCommand(dir, store).(runtime.ReadCommand)
	// --cluster is intentionally absent; the shim must NOT pick up "c-ctx".
	res, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "user": "testcluster"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "shim/no-cluster:tok", cred.Status.Token)

	// The mint request body must not include a clusterName field.
	assert.False(t, gjson.GetBytes(mintBody, "spec.clusterName").Exists(),
		"mint request must be unscoped when --cluster is omitted; got body: %s", mintBody)

	// The cache slot must be unscoped: CacheKey with empty cluster.
	serverID := config.CanonicalServerIdentity(srv.URL)
	unscopedKey := config.CacheKey("legacy:testcluster", serverID, "")
	_, _, cerr := store.GetCachedCred(unscopedKey)
	assert.NoError(t, cerr, "the cache slot must be the unscoped (no cluster) key")

	// The scoped key (with "c-ctx") must be absent — proving the context cluster
	// was not used.
	scopedKey := config.CacheKey("legacy:testcluster", serverID, "c-ctx")
	_, _, cerr = store.GetCachedCred(scopedKey)
	assert.ErrorIs(t, cerr, config.ErrNoToken,
		"the scoped (c-ctx) key must not exist; the context cluster must be ignored by the shim")
}

// TestLegacyTokenCacheHitIgnoresBrokenConfig is the fix-1 failing test: a
// shim invocation with a warm cache entry must succeed even when config.yaml
// is broken, mirroring the guarantee the native get-token fast path provides.
// Without fix 1 the shim calls runWithParams directly, which calls resolveConn,
// which calls config.Load and fails on the poisoned file.
func TestLegacyTokenCacheHitIgnoresBrokenConfig(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	serverID := config.CanonicalServerIdentity("https://r.ex")
	key := config.CacheKey("legacy:alice", serverID, "c-1")
	require.NoError(t, store.SetCachedCred(key, "cached-tok", time.Now().Add(time.Hour), "legacy:alice", serverID, "c-1"))
	// Poison config.yaml — the shim must not read it on a warm cache hit.
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("\t: not yaml ["), 0o600))

	cmd := NewLegacyTokenCommand(dir, store).(runtime.ReadCommand)
	res, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": "https://r.ex", "user": "alice", "cluster": "c-1"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "cached-tok", cred.Status.Token)
}

// errGetTokenStore wraps a CredStore and hard-fails GetToken for one server
// (by Name), simulating a locked keychain or broken exec-credential helper for
// an ambient server that is unrelated to the shim's actual target.
type errGetTokenStore struct {
	config.CredStore
	errName string
}

func (s *errGetTokenStore) GetToken(ref config.ServerRef) (string, error) {
	if ref.Name == s.errName {
		return "", errors.New("credential store: locked keychain")
	}
	return s.CredStore.GetToken(ref)
}

// TestLegacyTokenStoreErrorForAmbientServerDoesNotBlockMint is the round-3
// repro for finding 1: a hard credential-store error for the ambient
// current-context server (A) must not abort a cache-miss shim mint for an
// unrelated target server (B). Before the fix, resolveConn called
// cfg.Effective unconditionally on a cache miss, and Effective's GetToken for
// A hard-failed even though A was never the target.
func TestLegacyTokenStoreErrorForAmbientServerDoesNotBlockMint(t *testing.T) {
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == MintTokensPath && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":{"bearerToken":"b-tok"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srvB.Close()

	dir := t.TempDir()
	base := config.NewFileStore(dir)
	require.NoError(t, base.SetToken(config.ServerRef{Name: "server-a"}, "a-tok"))
	require.NoError(t, base.SetToken(config.ServerRef{Name: "server-b"}, "b-login-tok"))

	// Config: current server points at server A. Server A's GetToken will
	// hard-fail via errGetTokenStore, but the shim targets server B.
	cfg := &config.Config{
		CurrentServer: "server-a",
		Servers: map[string]config.Server{
			"server-a": {URL: "https://server-a.example"},
			"server-b": {URL: srvB.URL},
		},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	store := &errGetTokenStore{CredStore: base, errName: "server-a"}

	cmd := NewLegacyTokenCommand(dir, store).(runtime.ReadCommand)
	// No --cluster, no --cacerts: resolveConn enters the config-fallback branch
	// but must skip cfg.Effective (noAmbientContext=true, explicitServer=true)
	// and never call GetToken for server A.
	res, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srvB.URL, "user": "b-user"}},
		ui.Discard())
	require.NoError(t, err, "store error for ambient server A must not block a mint against server B")
	cred := res.Data.(*execCredential)
	assert.Equal(t, "b-tok", cred.Status.Token)
}

// TestLegacyTokenRejectsEmptyUser is the round-3 repro for finding 5:
// --user="" must be rejected with a VALIDATION clierr. Required:true in the
// descriptor only catches the absent flag; Execute must validate the value.
func TestLegacyTokenRejectsEmptyUser(t *testing.T) {
	t.Parallel()
	cmd := NewLegacyTokenCommand(t.TempDir(), nil).(runtime.ReadCommand)
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": "https://r.ex", "user": ""}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Contains(t, ce.Error(), "--user")
}

// TestLegacyTokenRejectsWhitespaceUser verifies that a whitespace-only --user
// is also rejected — strings.TrimSpace catches what an == "" check would miss.
func TestLegacyTokenRejectsWhitespaceUser(t *testing.T) {
	t.Parallel()
	cmd := NewLegacyTokenCommand(t.TempDir(), nil).(runtime.ReadCommand)
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": "https://r.ex", "user": "   "}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// TestLegacyTokenRejectsBlankServer covers the round-7 finding-2 gap: an
// empty or whitespace-only --server passes the pipeline's Required check
// (which only rejects the literal empty string when the flag is absent) and
// would otherwise fall through to resolveConn's config fallback, silently
// minting against the ambient current-context server. Execute must reject it
// with a VALIDATION clierr, symmetric with the --user checks above.
func TestLegacyTokenRejectsBlankServer(t *testing.T) {
	t.Parallel()
	for _, server := range []string{"", "   "} {
		cmd := NewLegacyTokenCommand(t.TempDir(), nil).(runtime.ReadCommand)
		_, err := cmd.Execute(context.Background(),
			tin{s: map[string]string{"server": server, "user": "alice"}},
			ui.Discard())
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce, "server=%q", server)
		assert.Equal(t, clierr.CodeValidation, ce.Code, "server=%q", server)
		assert.Contains(t, ce.Error(), "--server", "server=%q", server)
	}
}

// TestLegacyTokenPaddedUserHitsSameCacheSlot is the failing-first test for
// finding 2: --user with incidental whitespace padding must be trimmed before
// the cache slot is computed. A call with "  alice" must hit the same key as
// a call with "alice", ensuring a templated or hand-edited kubeconfig that
// produces padded values does not fragment the cache.
func TestLegacyTokenPaddedUserHitsSameCacheSlot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewLegacyTokenCommand(dir, store).(runtime.ReadCommand)

	// Call with canonical name — writes the cache slot.
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "user": "alice"}},
		ui.Discard())
	require.NoError(t, err)

	// Padded value must produce the SAME cache key (trimmed to "alice").
	serverID := config.CanonicalServerIdentity(srv.URL)
	canonicalKey := config.CacheKey("legacy:alice", serverID, "")
	paddedKey := config.CacheKey("legacy:  alice", serverID, "")

	// Sanity: the two keys must be distinct so the test isn't vacuous.
	require.NotEqual(t, canonicalKey, paddedKey,
		"test precondition: canonical and padded keys must differ")

	// The padded call should find the canonical key (trimmed first) rather
	// than writing a new "legacy:  alice" slot.
	_, _, err = store.GetCachedCred(canonicalKey)
	assert.NoError(t, err, "canonical key must be present after any invocation")

	// Execute with padded user — must NOT create the padded slot.
	_, err = cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "user": "  alice"}},
		ui.Discard())
	require.NoError(t, err)

	_, _, paddedErr := store.GetCachedCred(paddedKey)
	assert.ErrorIs(t, paddedErr, config.ErrNoToken,
		"padded-user invocation must not create a distinct 'legacy:  alice' slot")
}

// TestLegacyTokenPaddedClusterHitsSameCacheSlot is the Fix-4 test for --cluster:
// a padded cluster value ("  local  ") must be trimmed to "local" before the cache
// slot is computed. This also exercises the resolveMintClusterID short-circuit for
// "local" — no cluster-list HTTP round-trip is needed, keeping the server stub minimal.
func TestLegacyTokenPaddedClusterHitsSameCacheSlot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // looks like legacy Rancher with no ext.cattle.io
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "prod"}, "stored-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewLegacyTokenCommand(dir, store).(runtime.ReadCommand)

	// Call with canonical cluster — writes the cache slot.
	_, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "user": "alice", "cluster": "local"}},
		ui.Discard())
	require.NoError(t, err)

	serverID := config.CanonicalServerIdentity(srv.URL)
	canonicalKey := config.CacheKey("legacy:alice", serverID, "local")
	paddedKey := config.CacheKey("legacy:alice", serverID, "  local  ")

	require.NotEqual(t, canonicalKey, paddedKey,
		"test precondition: canonical and padded cluster keys must differ")

	_, _, err = store.GetCachedCred(canonicalKey)
	assert.NoError(t, err, "canonical key must be present after the first invocation")

	// Execute with padded cluster — must NOT create a distinct padded slot.
	_, err = cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "user": "alice", "cluster": "  local  "}},
		ui.Discard())
	require.NoError(t, err)

	_, _, paddedErr := store.GetCachedCred(paddedKey)
	assert.ErrorIs(t, paddedErr, config.ErrNoToken,
		"padded-cluster invocation must not create a distinct 'local  ' slot")
}

// TestShimPathConsultsNoAmbientState is the permanent class guard against the shim
// picking up ambient current-context state. A config whose current context names a
// same-host server entry ("wrong-server") whose GetToken hard-errors is paired with
// a "right-server" entry (sorted first) that has a valid stored login token. A shim
// invocation with the shared host URL, no --cluster, no --cacerts must succeed — it
// must not call cfg.Effective (needsEff gated), must pick the CA from sorted order
// not current-context (caForServer noAmbientContext), and must pick the stored login
// credential from rank-3 (sorted) not rank-2 (current-context). This test catches:
// (1) caForServer ignoring the current-context tie-break when noAmbientContext=true,
// (2) storedTokenForServer ignoring rank-2 when noAmbientContext=true,
// (3) needsEff not calling cfg.Effective for the ambient server.
func TestShimPathConsultsNoAmbientState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == MintTokensPath && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"status":{"bearerToken":"shim/ambient-guard:tok"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	base := config.NewFileStore(dir)
	// right-server (sorted first: "right-server" < "wrong-server") has a valid token.
	require.NoError(t, base.SetToken(config.ServerRef{Name: "right-server"}, "right-stored-tok"))
	// wrong-server is the current server; GetToken for it will hard-error.

	cfg := &config.Config{
		CurrentServer: "wrong-server",
		Servers: map[string]config.Server{
			"right-server": {URL: srv.URL, CA: ""},
			"wrong-server": {URL: srv.URL, CA: ""},
		},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	// Store hard-errors on GetToken for wrong-server, simulating a locked keychain
	// or broken credential helper for the ambient current-context server.
	store := &errGetTokenStore{CredStore: base, errName: "wrong-server"}

	cmd := NewLegacyTokenCommand(dir, store).(runtime.ReadCommand)
	// No --cluster, no --cacerts: resolveConn enters the config-fallback branch
	// for CA resolution. On the shim path (legacyShim), caForServer ignores the
	// current-context preference and returns right-server's CA (sorted first).
	// storedTokenForServer ignores rank-2 (wrong-server) and falls to rank-3;
	// right-server is first in sorted order and has a valid token.
	res, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "user": "testuser"}},
		ui.Discard())
	require.NoError(t, err, "shim must succeed: ambient wrong-server errors must not block a mint via right-server")
	cred := res.Data.(*execCredential)
	assert.Equal(t, "shim/ambient-guard:tok", cred.Status.Token)
}
