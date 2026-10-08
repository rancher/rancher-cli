package token

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestLegacyTokenDescriptor(t *testing.T) {
	t.Parallel()
	d := NewLegacyTokenCommand(t.TempDir(), nil, nil).Describe()
	assert.Equal(t, "token", d.Name)
	assert.Equal(t, runtime.Local, d.CommandType)
	assert.Equal(t, runtime.ServerAccessWrite, d.EffectiveServerAccess())
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

	cmd := NewLegacyTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)

	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "user": "alice"}},
		ui.Discard())
	require.NoError(t, err)

	_, err = cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "user": "bob"}},
		ui.Discard())
	require.NoError(t, err)

	serverID := config.CanonicalServerIdentity(srv.URL)
	keyAlice := config.CacheKey("legacy:alice", serverID, "")
	keyBob := config.CacheKey("legacy:bob", serverID, "")

	assert.NotEqual(t, keyAlice, keyBob, "different --user values must produce distinct cache slots")

	_, _, err = store.GetCachedCred(keyAlice, anchorFor(dir))
	require.NoError(t, err, "alice's cache slot must be written")

	_, _, err = store.GetCachedCred(keyBob, anchorFor(dir))
	assert.NoError(t, err, "bob's cache slot must be written")
}

// TestLegacyTokenEmitsV1Beta1 verifies the ExecCredential shape: Kind,
// APIVersion, and that ExpirationTimestamp is nil when the derived token create response
// carries no expiresAt (the zero-expiry case the shim opts into caching).
func TestLegacyTokenEmitsV1Beta1(t *testing.T) {
	// The create succeeds but returns no expiresAt - a zero-expiry token that
	// the native get-token would refuse to cache; the shim (legacyShim)
	// caches it and must not set ExpirationTimestamp in the emitted credential.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ExtTokensPath && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"legacy-created"},"status":{"bearerToken":"ext/legacy-created:secret"}}`))
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

	cmd := NewLegacyTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "user": "cluster-name"}},
		ui.Discard())
	require.NoError(t, err)

	assert.True(t, res.Raw, "ExecCredential result must be marked Raw")
	cred := res.Data.(*execCredential)
	assert.Equal(t, "ExecCredential", cred.Kind)
	assert.Equal(t, "client.authentication.k8s.io/v1beta1", cred.APIVersion)
	assert.Nil(t, cred.Status.ExpirationTimestamp, "no-expiry create response must not set ExpirationTimestamp")
}

// TestLegacyTokenAuthFlowEnvPrecedence verifies the three-level resolution:
// flag > RANCHER_CLI_OAUTH_AUTH_FLOW > CATTLE_OAUTH_AUTH_FLOW. The logic
// lives in resolveAuthFlow (oauth.go), the single source of truth
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
// stored current-context cluster when --cluster is omitted: the cache slot
// must be unscoped (empty cluster component) and the derived token create request body must
// carry no clusterName, regardless of whatever cluster the operator's local
// interactive CLI session has set.
func TestLegacyTokenNoContextCluster(t *testing.T) {
	var createBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == ExtTokensPath && r.Method == http.MethodPost:
			var err error
			createBody, err = io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"no-cluster"},"status":{"bearerToken":"ext/no-cluster:tok"}}`))
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

	cmd := NewLegacyTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)
	// --cluster is intentionally absent; the shim must NOT pick up "c-ctx".
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "user": "testcluster"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "ext/no-cluster:tok", cred.Status.Token)

	// The create request body must not include a clusterName field.
	assert.False(t, gjson.GetBytes(createBody, "spec.clusterName").Exists(),
		"create request must be unscoped when --cluster is omitted; got body: %s", createBody)

	// The cache slot must be unscoped: CacheKey with empty cluster.
	serverID := config.CanonicalServerIdentity(srv.URL)
	unscopedKey := config.CacheKey("legacy:testcluster", serverID, "")
	_, _, cerr := store.GetCachedCred(unscopedKey, anchorFor(dir))
	require.NoError(t, cerr, "the cache slot must be the unscoped (no cluster) key")

	// The scoped key (with "c-ctx") must be absent - proving the context cluster
	// was not used.
	scopedKey := config.CacheKey("legacy:testcluster", serverID, "c-ctx")
	_, _, cerr = store.GetCachedCred(scopedKey, anchorFor(dir))
	assert.ErrorIs(t, cerr, config.ErrNoToken,
		"the scoped (c-ctx) key must not exist; the context cluster must be ignored by the shim")
}

// TestLegacyTokenBrokenConfigMakesTheWarmHitAMiss asserts that a warm cache
// entry is a miss behind a broken config.yaml. The shim's fast path exists so
// the plugin serves a warm credential without a config load, but a record's
// owner is named in that file: the entry and its cache binding id. Serving
// one the shim cannot check means serving the previous user's credential
// after a re-login. The shim validates against the snapshot the root hook
// loaded, and no snapshot means no hit.
func TestLegacyTokenBrokenConfigMakesTheWarmHitAMiss(t *testing.T) {
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	serverID := config.CanonicalServerIdentity("https://r.ex")
	key := config.CacheKey("legacy:alice", serverID, "c-1")
	require.NoError(t, seedCache(t, dir, store, key, "cached-tok", time.Now().Add(time.Hour), "legacy:alice", serverID, "c-1"))

	anchor := config.NewCacheAnchor(dir)
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("\t: not yaml ["), 0o600))

	// Two shapes, because the shim can be reached in either. The root
	// hook clears the process anchor and only replaces it after a
	// successful load, so a shim dispatched behind an unparsable config
	// is handed nothing at all; a shim constructed with an anchor built
	// before the file broke holds one whose config is nil. Neither can
	// check a record's owner, so neither serves it.
	require.Nil(t, config.ProcessCacheAnchor(), "no test in this package leaves one set")
	_, _, _, hit, _ := shimProbeFlagCache(dir, "legacy:alice", "https://r.ex", "c-1", nil, ui.Discard())
	assert.False(t, hit, "with no anchor at all there is nothing to judge against")

	_, _, _, hit, _ = shimProbeFlagCache(dir, "legacy:alice", "https://r.ex", "c-1", anchor, ui.Discard())
	assert.False(t, hit, "a record whose binding cannot be checked is never served")

	// With the file readable again the same record serves, so the miss
	// above is the config error and nothing else.
	require.NoError(t, os.Remove(config.FilePath(dir)))
	_, _, r, hit, _ := shimProbeFlagCache(dir, "legacy:alice", "https://r.ex", "c-1", anchorFor(dir), ui.Discard())
	require.True(t, hit)
	cred := r.Data.(*execCredential)
	assert.Equal(t, "cached-tok", cred.Status.Token)
}

// TestLegacyTokenCacheHitSurfacesLoosePermissionNotice asserts that
// ReadOnlyCachedCred's notice return value (no store receiver exists on the
// shim's fast path to buffer it on instead) reaches the operator via
// Result.Warnings on a warm cache hit, exactly like any other warning on an
// ExecJSON shape.
func TestLegacyTokenCacheHitSurfacesLoosePermissionNotice(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("loose-permission notice is intentionally disabled on Windows; ACL enforcement is the guarantee there")
	}
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	serverID := config.CanonicalServerIdentity("https://r.ex")
	key := config.CacheKey("legacy:alice", serverID, "c-1")
	require.NoError(t, seedCache(t, dir, store, key, "cached-tok", time.Now().Add(time.Hour), "legacy:alice", serverID, "c-1"))
	require.NoError(t, os.Chmod(filepath.Join(dir, "token-cache.yaml"), 0o644))

	cmd := NewLegacyTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "https://r.ex", "user": "alice", "cluster": "c-1"}},
		ui.Discard())
	require.NoError(t, err)
	cred := res.Data.(*execCredential)
	assert.Equal(t, "cached-tok", cred.Status.Token)
	require.Len(t, res.Warnings, 1)
	assert.Contains(t, res.Warnings[0], "group/world-readable")
}

// errGetTokenStore wraps a CredStore and hard-fails GetToken for one server
// (by Name), simulating a locked keychain or broken exec-credential helper for
// a stored current server that is unrelated to the shim's actual target.
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

// TestLegacyTokenStoreErrorForAmbientServerDoesNotBlockCreate asserts that
// a hard credential-store error for the stored current
// current-context server (A) must not abort a cache-miss shim create for an
// unrelated target server (B). Resolving the connection reads no stored
// credential, so A's store is never asked.
func TestLegacyTokenStoreErrorForAmbientServerDoesNotBlockCreate(t *testing.T) {
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == ExtTokensPath && r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"b-tok"},"status":{"bearerToken":"ext/b-tok:secret"}}`))
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

	cmd := NewLegacyTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)
	// No --cluster, no --cacerts: resolveConn enters the config-fallback branch
	// but reads no credential (ignoreCurrentContext=true, explicitServer=true)
	// and never calls GetToken for server A.
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srvB.URL, "user": "b-user"}},
		ui.Discard())
	require.NoError(t, err, "store error for current server A must not block a derived token create against server B")
	cred := res.Data.(*execCredential)
	assert.Equal(t, "ext/b-tok:secret", cred.Status.Token)
}

// TestLegacyTokenRejectsEmptyUser asserts that
// --user="" is rejected with a VALIDATION clierr. Required:true in the
// descriptor only catches the absent flag; Execute must validate the value.
func TestLegacyTokenRejectsEmptyUser(t *testing.T) {
	t.Parallel()
	cmd := NewLegacyTokenCommand(t.TempDir(), nil, nil).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "https://r.ex", "user": ""}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.ErrorContains(t, ce, "--user")
}

// TestLegacyTokenRejectsWhitespaceUser verifies that a whitespace-only --user
// is also rejected - strings.TrimSpace catches what an == "" check would miss.
func TestLegacyTokenRejectsWhitespaceUser(t *testing.T) {
	t.Parallel()
	cmd := NewLegacyTokenCommand(t.TempDir(), nil, nil).(runtime.ReadCommand)
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": "https://r.ex", "user": "   "}},
		ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// TestLegacyTokenRejectsBlankServer covers the gap where an
// empty or whitespace-only --server passes the pipeline's Required check
// (which only rejects the literal empty string when the flag is absent) and
// would otherwise fall through to resolveConn's config fallback, silently
// creating against the stored current-context server. Execute must reject it
// with a VALIDATION clierr, symmetric with the --user checks above.
func TestLegacyTokenRejectsBlankServer(t *testing.T) {
	t.Parallel()
	for _, server := range []string{"", "   "} {
		cmd := NewLegacyTokenCommand(t.TempDir(), nil, nil).(runtime.ReadCommand)
		_, err := cmd.Execute(t.Context(),
			tin{s: map[string]string{"server": server, "user": "alice"}},
			ui.Discard())
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce, "server=%q", server)
		assert.Equal(t, clierr.CodeValidation, ce.Code, "server=%q", server)
		assert.ErrorContains(t, ce, "--server", "server=%q", server)
	}
}

// TestLegacyTokenPaddedUserHitsSameCacheSlot asserts that
// --user with incidental whitespace padding must be trimmed before
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

	cmd := NewLegacyTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)

	// Call with canonical name - writes the cache slot.
	_, err := cmd.Execute(t.Context(),
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
	_, _, err = store.GetCachedCred(canonicalKey, anchorFor(dir))
	require.NoError(t, err, "canonical key must be present after any invocation")

	// Execute with padded user - must NOT create the padded slot.
	_, err = cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "user": "  alice"}},
		ui.Discard())
	require.NoError(t, err)

	_, _, paddedErr := store.GetCachedCred(paddedKey, anchorFor(dir))
	assert.ErrorIs(t, paddedErr, config.ErrNoToken,
		"padded-user invocation must not create a distinct 'legacy:  alice' slot")
}

// TestLegacyTokenPaddedClusterHitsSameCacheSlot asserts, for --cluster, that
// a padded cluster value ("  local  ") must be trimmed to "local" before the cache
// slot is computed. This also exercises the resolveClusterIDForCreate short-circuit for
// "local" - no cluster-list HTTP round-trip is needed, keeping the server stub minimal.
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

	cmd := NewLegacyTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)

	// Call with canonical cluster - writes the cache slot.
	_, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "user": "alice", "cluster": "local"}},
		ui.Discard())
	require.NoError(t, err)

	serverID := config.CanonicalServerIdentity(srv.URL)
	canonicalKey := config.CacheKey("legacy:alice", serverID, "local")
	paddedKey := config.CacheKey("legacy:alice", serverID, "  local  ")

	require.NotEqual(t, canonicalKey, paddedKey,
		"test precondition: canonical and padded cluster keys must differ")

	_, _, err = store.GetCachedCred(canonicalKey, anchorFor(dir))
	require.NoError(t, err, "canonical key must be present after the first invocation")

	// Execute with padded cluster - must NOT create a distinct padded slot.
	_, err = cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "user": "alice", "cluster": "  local  "}},
		ui.Discard())
	require.NoError(t, err)

	_, _, paddedErr := store.GetCachedCred(paddedKey, anchorFor(dir))
	assert.ErrorIs(t, paddedErr, config.ErrNoToken,
		"padded-cluster invocation must not create a distinct 'local  ' slot")
}

// TestShimPathIgnoresCurrentContext asserts the shim never reads the current
// server's credential. A config whose current context names a server entry at
// another URL ("wrong-server") whose GetToken hard-errors is paired with a
// "right-server" entry at the call's URL that has a valid stored login token.
// A shim invocation with that URL, no --cluster, no --cacerts must succeed - it
// reads no stored credential while resolving the connection, so the current
// server's store failure cannot reach it. The same-URL half of the shim's independence from
// the current server (its ranking and its CA) is asserted by
// TestStoredTokenForServerShimIgnoresTheCurrentServer, the state table's shim
// rows and TestCaForServerDeterministicWithMultipleMatches.
func TestShimPathIgnoresCurrentContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == ExtTokensPath && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"unrelated-guard"},"status":{"bearerToken":"ext/unrelated-guard:tok"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	base := config.NewFileStore(dir)
	// right-server, at the call's URL, has a valid token.
	require.NoError(t, base.SetToken(config.ServerRef{Name: "right-server"}, "right-stored-tok"))
	// wrong-server is the current server, at another URL; GetToken for it will hard-error.

	cfg := &config.Config{
		CurrentServer: "wrong-server",
		Servers: map[string]config.Server{
			"right-server": {URL: srv.URL, CA: ""},
			"wrong-server": {URL: "https://wrong.example", CA: ""},
		},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	// Store hard-errors on GetToken for wrong-server, simulating a locked keychain
	// or broken credential helper for the stored current-context server.
	store := &errGetTokenStore{CredStore: base, errName: "wrong-server"}

	cmd := NewLegacyTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)
	// No --cluster, no --cacerts: resolveConn enters the config-fallback branch
	// for CA resolution without reading the current server's credential;
	// right-server is the only entry for the URL and has a valid token.
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "user": "testuser"}},
		ui.Discard())
	require.NoError(t, err, "shim must succeed: current-context wrong-server errors must not block a derived token create via right-server")
	cred := res.Data.(*execCredential)
	assert.Equal(t, "ext/unrelated-guard:tok", cred.Status.Token)
}

// noticeRecordingStore wraps a CredStore with a pre-loaded notice buffer,
// standing in for a store whose lazy resolution fell back to the plaintext
// file store and recorded the first-run keychain-unavailable notice.
type noticeRecordingStore struct {
	config.CredStore
	notices []string
}

func (s *noticeRecordingStore) DrainNotices() []string {
	out := s.notices
	s.notices = nil
	return out
}

// keychainUnavailableNotice mirrors the first-run wording the store records
// when the auto policy falls back to the plaintext file store.
const keychainUnavailableNotice = "keychain unavailable; credentials will be stored in plaintext"

// TestLegacyTokenStoreNoticeReachesStderr asserts the shim's notice delivery on
// success: the bare `token` command has no NoticeSource wired onto
// IO.Notices, so the shim drains its own store and the first-run
// keychain-unavailable notice reaches stderr as a "# warning:" line while
// stdout stays a bare ExecCredential object.
func TestLegacyTokenStoreNoticeReachesStderr(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	base := config.NewFileStore(dir)
	require.NoError(t, base.SetToken(config.ServerRef{Name: "prod"}, "stored-login-token"))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	store := &noticeRecordingStore{CredStore: base, notices: []string{keychainUnavailableNotice}}
	cmd := NewLegacyTokenCommand(dir, store, anchorFor(dir))

	var out, errBuf bytes.Buffer
	status, ce := runtime.Run(t.Context(), cmd,
		tin{s: map[string]string{"server": srv.URL, "user": "alice"}},
		runtime.IO{Out: &out, Err: &errBuf, In: strings.NewReader(""), NoInput: true},
		safety.Binding{})
	require.Nil(t, ce)
	assert.Zero(t, status)
	assert.Equal(t, "ExecCredential", gjson.GetBytes(out.Bytes(), "kind").String())
	assert.Contains(t, errBuf.String(), "# warning: "+keychainUnavailableNotice)
}

// TestLegacyTokenStoreNoticeRidesFailure asserts the failure-path counterpart:
// when the credential work fails after the store recorded a notice, the
// drained notice rides the error and still reaches stderr as a "# warning:"
// line ahead of the error text.
func TestLegacyTokenStoreNoticeRidesFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	// No stored login token: the shim falls through to the interactive
	// provider flow, whose provider listing 404s - a hard failure.
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	store := &noticeRecordingStore{CredStore: config.NewFileStore(dir), notices: []string{keychainUnavailableNotice}}
	cmd := NewLegacyTokenCommand(dir, store, anchorFor(dir))

	var out, errBuf bytes.Buffer
	_, ce := runtime.Run(t.Context(), cmd,
		tin{s: map[string]string{"server": srv.URL, "user": "alice"}},
		runtime.IO{Out: &out, Err: &errBuf, In: strings.NewReader(""), NoInput: true},
		safety.Binding{})
	require.NotNil(t, ce)
	assert.Equal(t, []string{keychainUnavailableNotice}, ce.Warnings())
	assert.Contains(t, errBuf.String(), "# warning: "+keychainUnavailableNotice)
	assert.Contains(t, errBuf.String(), "error (")
	assert.Empty(t, out.String(), "a failed credential run must write nothing to stdout")
}

// TestLegacyTokenProbeWarnRidesLoadFailure asserts the probe's loose-permission
// finding surviving a slow-path failure: the probe's global dedup marks the
// cache path seen on first check, so the warning has no second chance - a
// cache miss followed by a broken config.yaml must still carry it on the
// error.
func TestLegacyTokenProbeWarnRidesLoadFailure(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("loose-permission notice is intentionally disabled on Windows; ACL enforcement is the guarantee there")
	}
	dir := t.TempDir()
	seed := config.NewFileStore(dir)
	serverID := config.CanonicalServerIdentity("https://r.ex")
	otherKey := config.CacheKey("legacy:bob", serverID, "c-1")
	require.NoError(t, seedCache(t, dir, seed, otherKey, "cached-tok", time.Now().Add(time.Hour), "legacy:bob", serverID, "c-1"))
	require.NoError(t, os.Chmod(filepath.Join(dir, "token-cache.yaml"), 0o644))
	require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("{not yaml"), 0o600))

	cmd := NewLegacyTokenCommand(dir, nil, anchorFor(dir))
	var out, errBuf bytes.Buffer
	_, ce := runtime.Run(t.Context(), cmd,
		tin{s: map[string]string{"server": "https://r.ex", "user": "alice", "cluster": "c-1"}},
		runtime.IO{Out: &out, Err: &errBuf, In: strings.NewReader(""), NoInput: true},
		safety.Binding{})
	require.NotNil(t, ce)
	require.Len(t, ce.Warnings(), 1)
	assert.Contains(t, ce.Warnings()[0], "group/world-readable")
	assert.Contains(t, errBuf.String(), "# warning: ")
	assert.Contains(t, errBuf.String(), "group/world-readable")
}

// TestLegacyTokenProbeWarnPrecedesStoreNotices asserts delivery in record
// order when both the fast-path probe and the slow-path store produce
// notices: the probe observed its loose-permission finding before any store
// work, so it must come first on the error's warning ride.
func TestLegacyTokenProbeWarnPrecedesStoreNotices(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("loose-permission notice is intentionally disabled on Windows; ACL enforcement is the guarantee there")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	seed := config.NewFileStore(dir)
	serverID := config.CanonicalServerIdentity(srv.URL)
	otherKey := config.CacheKey("legacy:bob", serverID, "c-1")
	require.NoError(t, seedCache(t, dir, seed, otherKey, "cached-tok", time.Now().Add(time.Hour), "legacy:bob", serverID, "c-1"))
	require.NoError(t, os.Chmod(filepath.Join(dir, "token-cache.yaml"), 0o644))
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	store := &noticeRecordingStore{CredStore: config.NewFileStore(dir), notices: []string{keychainUnavailableNotice}}
	cmd := NewLegacyTokenCommand(dir, store, anchorFor(dir))
	var out, errBuf bytes.Buffer
	_, ce := runtime.Run(t.Context(), cmd,
		tin{s: map[string]string{"server": srv.URL, "user": "alice", "cluster": "c-1"}},
		runtime.IO{Out: &out, Err: &errBuf, In: strings.NewReader(""), NoInput: true},
		safety.Binding{})
	require.NotNil(t, ce)
	warnings := ce.Warnings()
	require.Len(t, warnings, 2)
	assert.Contains(t, warnings[0], "group/world-readable", "the probe's earlier finding delivers first")
	assert.Equal(t, keychainUnavailableNotice, warnings[1])
}

// TestShimWithClusterUsesThePluginLoginUnderTheSetting asserts an ACE
// kubeconfig invocation (--cluster set) that misses cache runs the
// plugin login when the setting is on, with ignoreCurrentContext kept:
// the entry is created and CurrentServer stays empty.
func TestShimWithClusterUsesThePluginLoginUnderTheSetting(t *testing.T) {
	x := newPluginFixture(t, true, testrancher.Options{})
	cmd := NewLegacyTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
	res, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": x.srv.URL, "user": "alice", "cluster": "c-test", "auth-provider": "localProvider"}}, localPrompter())
	require.NoError(t, err)
	assert.Equal(t, "ExecCredential", res.Kind)
	assert.Equal(t, 1, x.f.LoginCount())
	s, ok := x.entry(t)
	require.True(t, ok)
	assert.Equal(t, clierr.ProvenanceExecLogin, s.Provenance)
	cfg, _ := config.Load(config.FilePath(x.dir))
	assert.Empty(t, cfg.CurrentServer, "the plugin login never sets CurrentServer")
}

// TestShimNonACEUnscopedCreateUnchangedUnderTheSetting asserts parity: a
// shim call without --cluster creates the unscoped kubeconfig token as
// today with the setting on and off, and never a plugin login.
func TestShimNonACEUnscopedCreateUnchangedUnderTheSetting(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		x := newPluginFixture(t, reuse, testrancher.Options{})
		cmd := NewLegacyTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
		_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": x.srv.URL, "user": "alice", "auth-provider": "localProvider"}}, localPrompter())
		require.NoError(t, err)
		_, present := x.entry(t)
		assert.False(t, present)
		creates := x.f.CreateRequests()
		require.Len(t, creates, 1)
		assert.Equal(t, "session", creates[0].Role)
		assert.Empty(t, creates[0].ClusterName)
	}
}
