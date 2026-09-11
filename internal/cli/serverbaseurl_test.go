package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServerBaseURLConfiguredServer asserts the 3c port against the ordinary
// configured-server path: the same scheme+host Root()'s rootClient.base
// carries, no trailing slash.
func TestServerBaseURLConfiguredServer(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	f := seedRootConfig(t, srv)
	base, err := f.ServerBaseURL()
	require.NoError(t, err)
	assert.Equal(t, srv.URL, base)
}

// TestServerBaseURLEnvOverride asserts RANCHER_CLI_URL: the override clears the
// stored server name (Identity), but ServerBaseURL must still resolve the
// EFFECTIVE (overridden) base - never the stale configured one.
func TestServerBaseURLEnvOverride(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	f := seedRootConfig(t, srv)
	t.Setenv(config.EnvURL, srv.URL+"/")
	base, err := f.ServerBaseURL()
	require.NoError(t, err)
	assert.Equal(t, srv.URL, base, "trailing slash must not survive into the base")
}

// TestServerBaseURLHTTP asserts a plain-http development server: scheme must
// be preserved, never upgraded/assumed https.
func TestServerBaseURLHTTP(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(
		"currentServer: acme\nservers:\n  acme:\n    url: http://127.0.0.1:9999\n"), 0o600))
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme"}, "token-abc:secret"))
	f := newClientFactory(dir, store)
	base, err := f.ServerBaseURL()
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:9999", base)
}
