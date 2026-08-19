package token

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	rt "github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildTokenFakeHelper compiles the config package's fake credential helper by
// module path (testdata is not reachable by relative path from this package)
// into a temp dir as rancher-credential-fake, and returns the directory for
// PATH injection.
func buildTokenFakeHelper(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "rancher-credential-fake")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	out, err := exec.Command("go", "build", "-o", bin,
		"github.com/rancher/rancher-cli/internal/config/testdata/fakehelper").CombinedOutput()
	require.NoError(t, err, "building fake helper: %s", out)
	return dir
}

// useTokenFakeHelper puts the fake helper on PATH in the given mode with a
// fresh state file.
func useTokenFakeHelper(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("PATH", buildTokenFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEHELPER_MODE", mode)
	t.Setenv("FAKEHELPER_STATE", filepath.Join(t.TempDir(), "state.json"))
}

// TestGetTokenLegacyFallbackDoesNotPersistShortLivedHelperToken is the
// impl-review finding-1 repro: when the credential store is a helper serving a
// SHORT-LIVED token and get-token falls back to the legacy (pre-ext.cattle.io)
// path, the returned credential must carry the helper's REAL expiry (never the
// fabricated now+legacyMintTTL) and must never be written to the on-disk cache.
func TestGetTokenLegacyFallbackDoesNotPersistShortLivedHelperToken(t *testing.T) {
	useTokenFakeHelper(t, "expiresat")
	helperExp := time.Now().Add(2 * time.Minute).UTC() // sooner than legacyMintTTL (15m)
	t.Setenv("FAKEHELPER_EXPIRESAT", helperExp.Format(time.RFC3339))

	// A legacy Rancher: no ext.cattle.io served, so the mint 404s.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	store, err := config.OpenStore("exec:fake", dir)
	require.NoError(t, err)
	cfg := &config.Config{CredentialStore: "exec:fake", Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(rt.ReadCommand)
	res, err := cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.Discard())
	require.NoError(t, err)

	cred := res.Data.(*execCredential)
	assert.Equal(t, "short-lived", cred.Status.Token)
	require.NotNil(t, cred.Status.ExpirationTimestamp, "the credential must carry an expiry")
	assert.WithinDuration(t, helperExp, *cred.Status.ExpirationTimestamp, 2*time.Second,
		"the returned expiry must be the helper's real one, not now+legacyMintTTL")
	assert.False(t, cred.Status.ExpirationTimestamp.After(time.Now().Add(legacyMintTTL)),
		"a short-lived helper token must never be extended to the legacy TTL")

	_, _, cerr := store.GetCachedCred(config.CacheKey("prod", config.CanonicalServerIdentity(srv.URL), "local"))
	assert.ErrorIs(t, cerr, config.ErrNoToken, "a memory-only short-lived helper token must not reach the disk cache")
}

// TestGetTokenSurfacesHardHelperError is the impl-review finding-3 repro: a
// hard helper failure (a helper that exits non-zero, or returns garbage) must
// surface as the real error instead of being swallowed as a credential miss
// and papered over by the interactive fallback.
func TestGetTokenSurfacesHardHelperError(t *testing.T) {
	for _, mode := range []string{"fail", "garbage"} {
		t.Run(mode, func(t *testing.T) {
			useTokenFakeHelper(t, mode)

			var authProviderHits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1-public/authproviders" {
					authProviderHits.Add(1)
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer srv.Close()

			dir := t.TempDir()
			store, err := config.OpenStore("exec:fake", dir)
			require.NoError(t, err)
			cfg := &config.Config{CredentialStore: "exec:fake", Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())

			cmd := NewTokenCommand(dir, store).(rt.ReadCommand)
			_, err = cmd.Execute(context.Background(),
				tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "c-1"}},
				ui.Discard())
			require.Error(t, err, "a hard helper failure must not be swallowed")
			assert.Contains(t, err.Error(), "credential helper", "the real helper error must surface")
			assert.Equal(t, int32(0), authProviderHits.Load(),
				"a hard helper failure must not fall through to the interactive flow")
		})
	}
}

// TestGetTokenFTPConfigServerHelperNeverInvoked is the final-re-review
// finding-2 repro at the get-token level: when the config's current server
// URL uses an unsupported scheme (ftp://), Config.Effective must not invoke
// the exec credential helper before post-resolution validation rejects it.
//
// The fakehelper is in "ok" mode — any invocation writes a get entry to the
// state log (creating the file). The state file absent after the command
// proves zero helper calls.
func TestGetTokenFTPConfigServerHelperNeverInvoked(t *testing.T) {
	useTokenFakeHelper(t, "ok")
	statePath := os.Getenv("FAKEHELPER_STATE")

	dir := t.TempDir()
	store, err := config.OpenStore("exec:fake", dir)
	require.NoError(t, err)
	cfg := &config.Config{
		CredentialStore: "exec:fake",
		CurrentServer:   "prod",
		Servers:         map[string]config.Server{"prod": {URL: "ftp://host"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(rt.ReadCommand)
	_, err = cmd.Execute(context.Background(),
		tin{s: map[string]string{"name": "prod", "cluster": "c-1"}},
		ui.Discard())
	require.Error(t, err, "ftp:// config server must be rejected by post-resolution validation")
	assert.Contains(t, err.Error(), "ftp://host", "error must name the invalid resolved server")

	// The state file is created only on helper invocation. If it is absent,
	// the helper was never called.
	_, statErr := os.Stat(statePath)
	assert.True(t, os.IsNotExist(statErr),
		"exec helper must not be invoked for a config server URL with an unsupported scheme")
}

// TestGetTokenHelperNotFoundFallsBackToInteractive is finding-3's other half:
// a clean not-found from the helper (no token) is NOT a hard error and must
// still fall through to the interactive provider flow.
func TestGetTokenHelperNotFoundFallsBackToInteractive(t *testing.T) {
	useTokenFakeHelper(t, "notfound")

	var authProviderHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1-public/authproviders" {
			authProviderHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"type":"pingProvider"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	store, err := config.OpenStore("exec:fake", dir)
	require.NoError(t, err)
	cfg := &config.Config{CredentialStore: "exec:fake", Servers: map[string]config.Server{"prod": {URL: srv.URL}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := NewTokenCommand(dir, store).(rt.ReadCommand)
	_, err = cmd.Execute(context.Background(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "c-1"}},
		ui.Discard())
	// SAML under a non-interactive prompter fast-fails AUTH — the point is that
	// the flow reached the interactive provider dance at all.
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, int32(1), authProviderHits.Load(),
		"a clean helper miss must fall through to the interactive flow")
}
