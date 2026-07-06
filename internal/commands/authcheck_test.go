package commands

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	res, err := NewAuthCheck(dir, store).Execute(context.Background(), args{}, safety.Effect{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "AuthCheck", res.Kind)
}
