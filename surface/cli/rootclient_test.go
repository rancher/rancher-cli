package cli

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pemFromServer encodes srv's TLS certificate as a CERTIFICATE PEM block —
// the same encoding e2e/harness_test.go uses for its TLS shim's CA material.
func pemFromServer(t *testing.T, srv *httptest.Server) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

// seedRootConfig points the factory at srv with a stored token and returns
// the factory. Uses a TLS test server + its cert as --cacert material so
// the real httpx CA path is exercised.
func seedRootConfig(t *testing.T, srv *httptest.Server) *clientFactory {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(certPath, pemFromServer(t, srv), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(
		"current-context: acme\nservers:\n  acme:\n    url: "+srv.URL+"\n    ca: "+certPath+"\ncontexts:\n  acme:\n    server: acme\n"), 0o600))
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme"}, "token-abc:secret"))
	return newClientFactory(dir, store)
}

func TestRootClientSendsBearerAndCA(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	rc, err := seedRootConfig(t, srv).Root()
	require.NoError(t, err)
	body, err := rc.GetJSON(context.Background(), "/v1-rke2-release/release")
	require.NoError(t, err)
	assert.Contains(t, string(body), `"ok"`)
	assert.Equal(t, "Bearer token-abc:secret", gotAuth)
	assert.Equal(t, "/v1-rke2-release/release", gotPath)
}

func TestRootClientRejectsUnknownCA(t *testing.T) {
	// review R1-4: spec D-10 names bad-CA as a required transport case.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	// Seed config WITHOUT the server's cert as CA (self-signed unknown CA).
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(
		"current-context: acme\nservers:\n  acme:\n    url: "+srv.URL+"\ncontexts:\n  acme:\n    server: acme\n"), 0o600))
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme"}, "t:s"))
	rc, err := newClientFactory(dir, store).Root()
	require.NoError(t, err)
	_, err = rc.GetJSON(context.Background(), "/x")
	require.Error(t, err, "unknown CA must fail TLS verification, not fall back")
}

func TestRootClientTimesOut(t *testing.T) {
	// review R1-4: timeout case. Server hangs past the client deadline; the
	// test injects a short timeout via the factory seam rather than waiting
	// out the 30s default — a package-var/option seam for the timeout
	// (mirroring keyringProbeTimeout's test-shrinkable pattern in
	// internal/config/credstore.go). Not run with t.Parallel(): it mutates
	// the shared rootTimeout package var, and this package's other
	// t.Parallel() tests must not observe a shrunk timeout mid-run.
	hang := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { <-hang }))
	defer func() { close(hang); srv.Close() }()
	f := seedRootConfig(t, srv)
	restore := setRootTimeoutForTest(t, 100*time.Millisecond) // the seam this task adds
	defer restore()
	rc, err := f.Root()
	require.NoError(t, err)
	_, err = rc.GetJSON(context.Background(), "/slow")
	require.Error(t, err)
}

func TestRootClientMapsStatusToClierr(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gone":
			http.Error(w, "nope", http.StatusNotFound)
		case "/denied":
			http.Error(w, "nope", http.StatusUnauthorized)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	rc, err := seedRootConfig(t, srv).Root()
	require.NoError(t, err)
	var ce *clierr.Error

	_, err = rc.GetJSON(context.Background(), "/gone")
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)

	_, err = rc.GetJSON(context.Background(), "/denied")
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeAuth, ce.Code)

	_, err = rc.GetJSON(context.Background(), "/boom")
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code)
	assert.Contains(t, ce.Message, "boom", "generic error carries a body excerpt")
}

// setRootTimeoutForTest shrinks the rootClient HTTP timeout for the duration
// of a test, mirroring internal/config/credstore.go's keyringProbeTimeout
// test-shrinkable pattern. Must be called before the factory's first Root()
// (the timeout is baked into the memoized http.Client at construction).
func setRootTimeoutForTest(t *testing.T, d time.Duration) (restore func()) {
	t.Helper()
	orig := rootTimeout
	rootTimeout = d
	return func() { rootTimeout = orig }
}
