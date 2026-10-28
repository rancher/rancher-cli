package token

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
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

// firstRunLimit bounds the fake helper's first run, so a hung helper fails
// the package with a message.
const firstRunLimit = 5 * time.Minute

// buildTokenFakeHelper compiles the config package's fake credential helper by
// module path (testdata is not reachable by relative path from this package)
// into a shared temp dir as rancher-credential-fake, once per test binary,
// and returns the directory for PATH injection. TestMain removes the dir.
var (
	fakeHelperOnce sync.Once
	fakeHelperDir  string
	fakeHelperErr  error
)

func buildTokenFakeHelper(t *testing.T) string {
	t.Helper()
	fakeHelperOnce.Do(func() {
		dir, err := os.MkdirTemp("", "token-fakehelper") //nolint:usetesting // shared by every test of the binary; TestMain removes it
		if err != nil {
			fakeHelperErr = err
			return
		}
		fakeHelperDir = dir // set before the build, so TestMain removes it on a failure too
		bin := filepath.Join(dir, "rancher-credential-fake")
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		if out, err := exec.Command("go", "build", "-o", bin,
			"github.com/rancher/rancher-cli/internal/config/testdata/fakehelper").CombinedOutput(); err != nil {
			fakeHelperErr = fmt.Errorf("building fake helper: %w: %s", err, out)
			return
		}
		// A new binary's first run can take many seconds under load; run it
		// here, outside any helper time limit.
		ctx, cancel := context.WithTimeout(t.Context(), firstRunLimit)
		defer cancel()
		if out, err := exec.CommandContext(ctx, bin, "noop").CombinedOutput(); err != nil {
			if ctx.Err() != nil {
				err = errors.New("did not finish within " + firstRunLimit.String())
			}
			fakeHelperErr = fmt.Errorf("first run of the fake helper: %w: %s", err, out)
			return
		}
	})
	require.NoError(t, fakeHelperErr)
	return fakeHelperDir
}

// useTokenFakeHelper puts the fake helper on PATH in the given mode with a
// fresh state file.
func useTokenFakeHelper(t *testing.T, mode string) {
	t.Helper()
	t.Setenv("PATH", buildTokenFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEHELPER_MODE", mode)
	t.Setenv("FAKEHELPER_STATE", filepath.Join(t.TempDir(), "state.json"))
}

// TestGetTokenLegacyFallbackDoesNotPersistShortLivedHelperToken asserts
// that when the credential store is a helper serving a
// SHORT-LIVED token and get-token falls back to the legacy (pre-ext.cattle.io)
// path, the returned credential must carry the helper's REAL expiry (never the
// fabricated now+legacyTokenTTL) and must never be written to the on-disk cache.
func TestGetTokenLegacyFallbackDoesNotPersistShortLivedHelperToken(t *testing.T) {
	useTokenFakeHelper(t, "expiresat")
	helperExp := time.Now().Add(2 * time.Minute).UTC() // sooner than legacyTokenTTL (15m)
	t.Setenv("FAKEHELPER_EXPIRESAT", helperExp.Format(time.RFC3339))

	// A legacy Rancher: no ext.cattle.io served, so the derived token create 404s.
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

	cmd := newTokenCommandForTest(t, dir, store).(rt.ReadCommand)
	res, err := cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "local"}},
		ui.Discard())
	require.NoError(t, err)

	cred := res.Data.(*execCredential)
	assert.Equal(t, "short-lived", cred.Status.Token)
	require.NotNil(t, cred.Status.ExpirationTimestamp, "the credential must carry an expiry")
	assert.WithinDuration(t, helperExp, *cred.Status.ExpirationTimestamp, 2*time.Second,
		"the returned expiry must be the helper's real one, not now+legacyTokenTTL")
	assert.False(t, cred.Status.ExpirationTimestamp.After(time.Now().Add(legacyTokenTTL)),
		"a short-lived helper token must never be extended to the legacy TTL")

	_, _, cerr := store.GetCachedCred(config.CacheKey("prod", config.CanonicalServerIdentity(srv.URL), "local"), anchorFor(dir))
	assert.ErrorIs(t, cerr, config.ErrNoToken, "a memory-only short-lived helper token must not reach the disk cache")
}

// TestGetTokenSurfacesHardHelperError asserts that a
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

			cmd := newTokenCommandForTest(t, dir, store).(rt.ReadCommand)
			_, err = cmd.Execute(t.Context(),
				tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "c-1"}},
				ui.Discard())
			require.Error(t, err, "a hard helper failure must not be swallowed")
			require.ErrorContains(t, err, "credential helper", "the real helper error must surface")
			assert.Equal(t, int32(0), authProviderHits.Load(),
				"a hard helper failure must not fall through to the interactive flow")
		})
	}
}

// TestGetTokenFTPConfigServerHelperNeverInvoked asserts, at the get-token
// level, that when the config's current server URL uses an unsupported scheme
// (ftp://), the exec credential helper is not invoked: resolving the connection
// reads no stored credential, and post-resolution validation rejects the scheme
// before any lookup.
//
// The fakehelper is in "ok" mode - any invocation writes a get entry to the
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

	cmd := newTokenCommandForTest(t, dir, store).(rt.ReadCommand)
	_, err = cmd.Execute(t.Context(),
		tin{s: map[string]string{"name": "prod", "cluster": "c-1"}},
		ui.Discard())
	require.Error(t, err, "ftp:// config server must be rejected by post-resolution validation")
	require.ErrorContains(t, err, "the server URL in server entry 'prod' cannot be used: its scheme is not http or https; change the entry's URL with "+
		clierr.Rancher+" config update-server 'prod' --url <url>")
	assert.NotContains(t, err.Error(), "ftp://host", "the error holds no part of the URL")

	// The state file is created only on helper invocation. If it is absent,
	// the helper was never called.
	_, statErr := os.Stat(statePath)
	assert.True(t, os.IsNotExist(statErr),
		"exec helper must not be invoked for a config server URL with an unsupported scheme")
}

// TestGetTokenHelperNotFoundFallsBackToInteractive asserts the counterpart of
// TestGetTokenSurfacesHardHelperError: a clean not-found from the helper (no
// token) is NOT a hard error and must still fall through to the interactive
// provider flow.
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

	cmd := newTokenCommandForTest(t, dir, store).(rt.ReadCommand)
	_, err = cmd.Execute(t.Context(),
		tin{s: map[string]string{"server": srv.URL, "name": "prod", "cluster": "c-1"}},
		ui.Discard())
	// SAML under a non-interactive prompter fast-fails AUTH - the point is that
	// the flow reached the interactive provider dance at all.
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, int32(1), authProviderHits.Load(),
		"a clean helper miss must fall through to the interactive flow")
}
