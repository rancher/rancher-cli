package cli

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
)

// managementClusterServer serves the management cluster list at the exact
// path the cluster source reads, counting hits so zero-request contracts
// are provable.
func managementClusterServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/k8s/clusters/local/apis/management.cattle.io/v3/clusters" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"c-abc"},"spec":{"displayName":"prod"}},
			{"metadata":{"name":"c-def"},"spec":{"displayName":"staging"}}
		]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestInspectClusterPositionalGateOff proves the network gate governs the
// exception exactly as it governs every live site: completion.network
// false → nil candidates, zero requests, before any dispatch.
func TestInspectClusterPositionalGateOff(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	srv, hits := managementClusterServer(t)
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: false},
	}
	setStoredConfigForTest(t, cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "inspect cluster"}
	got := dispatchSite(t.Context(), site, d, nil, nil, nil)
	assert.Nil(t, got)
	assert.Zero(t, hits.Load(), "gate off: zero requests")
}
