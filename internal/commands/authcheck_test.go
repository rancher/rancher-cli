package commands

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuthCheckUsesInjectedPreflight exercises Execute with a stub preflight so
// the command's result-shaping and error propagation are covered without a
// live server.
func TestAuthCheckUsesInjectedPreflight(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, pf func(context.Context, config.Effective) (string, error)) (runtime.Result, error) {
		t.Helper()
		dir := t.TempDir()
		cmd := &authCheckCmd{dir: dir, store: config.NewFileStore(dir), preflight: pf}
		return cmd.Execute(context.Background(), args{}, ui.Discard())
	}

	t.Run("success returns an AuthCheck result", func(t *testing.T) {
		t.Parallel()
		res, err := run(t, func(context.Context, config.Effective) (string, error) {
			return "v1.31.0", nil
		})
		require.NoError(t, err)
		assert.Equal(t, "AuthCheck", res.Kind)
		assert.Equal(t, "v1.31.0", res.Data.(map[string]any)["serverVersion"])
	})

	t.Run("auth failure propagates the AUTH code", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, func(context.Context, config.Effective) (string, error) {
			return "", clierr.New(clierr.CodeAuth, "bad token")
		})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
	})

	t.Run("unreachable server propagates GENERIC retryable", func(t *testing.T) {
		t.Parallel()
		_, err := run(t, func(context.Context, config.Effective) (string, error) {
			return "", clierr.New(clierr.CodeGeneric, "cannot reach server").WithRetryable(true)
		})
		var ce *clierr.Error
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, clierr.CodeGeneric, ce.Code)
		assert.True(t, ce.Retryable)
	})
}

func TestAuthCheckHitsServerVersion(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/k8s/clusters/local/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"major":"1","minor":"31","gitVersion":"v1.31.0"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetToken("acme", "id:secret"))
	// Extract the test server's CA cert so RESTConfig can verify it (secure-by-default).
	cert := srv.Certificate()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	cfg := &config.Config{
		CurrentContext: "acme",
		Contexts:       map[string]config.Context{"acme": {Server: "acme", Cluster: "local"}},
		Servers:        map[string]config.Server{"acme": {URL: srv.URL, CA: string(certPEM)}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	res, err := NewAuthCheck(dir, store).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "AuthCheck", res.Kind)
}
