package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
)

// clusterFamilyPoolServer serves ONE named provisioning Cluster's
// spec.rkeConfig.machinePools, for the dependent --pool flag completion. A
// second path serves a different namespace so the --namespace override test
// can assert it is used instead of fleet-default.
func clusterFamilyPoolServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-default/clusters/my-cluster", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"spec":{"rkeConfig":{"machinePools":[
			{"name":"pool1"},{"name":"pool2"},{"name":"evil;rm -rf"}
		]}}}`))
	})
	mux.HandleFunc("/k8s/clusters/local/apis/provisioning.cattle.io/v1/namespaces/fleet-west/clusters/my-cluster", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"spec":{"rkeConfig":{"machinePools":[{"name":"west-pool"}]}}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestLivePositionalClusterFamilyClusterName pins clusterFamilyPositionalKind:
// every cluster-family bespoke verb's <cluster> positional lists
// provisioning-cluster names via the SAME fleet-default path
// get/delete provisioning-cluster use, keyed off the op name rather than
// kindForDescriptor's last-token derivation (which would otherwise resolve
// the unrelated management "cluster" kind for "cluster scale").
func TestLivePositionalClusterFamilyClusterName(t *testing.T) {
	t.Parallel()
	for _, opName := range []string{
		"cluster scale", "cluster set-autoscaling", "cluster clear-autoscaling",
		"cluster pause-autoscaling", "cluster resume-autoscaling",
	} {
		t.Run(opName, func(t *testing.T) {
			t.Parallel()
			srv, hits := provisioningClusterServer(t)
			cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
			site := CompletionSite{Kind: SitePositional, Index: 0}
			d := runtime.Descriptor{Name: opName}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
			assert.Contains(t, got, "my-rke2")
			assert.Contains(t, got, "my-k3s")
			assert.Equal(t, int32(1), hits.Load())
		})
	}
}

// TestLivePositionalClusterFamilyClusterNameNamespaceOverride confirms a
// --namespace flag in the arg tail overrides the fleet-default fallback for
// the family's <cluster> positional, same as the generic case.
func TestLivePositionalClusterFamilyClusterNameNamespaceOverride(t *testing.T) {
	t.Parallel()
	srv, hits := provisioningClusterServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "cluster scale"}
	tail := []string{"--namespace", "fleet-west"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", tail)
	assert.Contains(t, got, "ns-cluster")
	assert.Equal(t, int32(1), hits.Load())
}

// TestLivePositionalClusterFamilyClusterNameIDOnlyBoundary pins the
// management-token boundary for the family's <cluster> positional: an
// exec-cred-only capability cannot authorize the management-plane list.
func TestLivePositionalClusterFamilyClusterNameIDOnlyBoundary(t *testing.T) {
	t.Parallel()
	srv, hits := provisioningClusterServer(t)
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "cluster scale"}
	cap := TokenCapability{Token: "downstream", IDOnly: true, ServerURL: srv.URL}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
	cancel()
	assert.Nil(t, got)
	assert.Zero(t, hits.Load())
}

// clusterFamilyPoolDescriptor is the minimal descriptor shape
// clusterFamilyPoolCandidates needs: an <cluster> positional at index 0 (so
// firstPositionalToken finds it) and no other leaf flags to consume.
var clusterFamilyPoolDescriptor = runtime.Descriptor{
	Name: "cluster scale",
	Args: []runtime.ArgSpec{{Name: "cluster", Type: "string", Required: true}},
	Flags: []runtime.ArgSpec{
		{Name: "pool", Type: "string"},
		{Name: "namespace", Type: "string"},
	},
}

// setClusterFamilyCompletionConfig points completion at srv with network
// completion enabled and a management token — the same stored-config/env
// setup logsPodCandidates' own tests use, since clusterFamilyPoolCandidates
// resolves its capability via resolveCompletionToken internally rather than
// taking one as a parameter (self-contained, like logsPodCandidates).
func setClusterFamilyCompletionConfig(t *testing.T, srv *httptest.Server) {
	t.Helper()
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")
}

// TestLiveFlagValueClusterFamilyPool pins the DEPENDENT --pool flag site: a
// client-scoped GET of the already-typed <cluster> positional's
// spec.rkeConfig.machinePools names, hostile names sanitized, suggestions
// valid verbatim.
// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
func TestLiveFlagValueClusterFamilyPool(t *testing.T) {
	srv, hits := clusterFamilyPoolServer(t)
	setClusterFamilyCompletionConfig(t, srv)

	site := CompletionSite{Kind: SiteFlagValue, FlagName: "pool", FlagSpelling: "--pool"}
	tail := []string{"my-cluster", "--pool"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := liveCandidates(ctx, site, clusterFamilyPoolDescriptor, nil, tail)
	assert.Equal(t, []string{"pool1", "pool2"}, got)
	assert.Equal(t, int32(1), hits.Load())
}

// TestLiveFlagValueClusterFamilyPoolNamespaceOverride confirms --pool
// completion honors a --namespace override on the tail, same as the
// <cluster> positional.
// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
func TestLiveFlagValueClusterFamilyPoolNamespaceOverride(t *testing.T) {
	srv, hits := clusterFamilyPoolServer(t)
	setClusterFamilyCompletionConfig(t, srv)

	site := CompletionSite{Kind: SiteFlagValue, FlagName: "pool", FlagSpelling: "--pool"}
	tail := []string{"my-cluster", "--namespace", "fleet-west", "--pool"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := liveCandidates(ctx, site, clusterFamilyPoolDescriptor, nil, tail)
	assert.Equal(t, []string{"west-pool"}, got)
	assert.Equal(t, int32(1), hits.Load())
}

// TestLiveFlagValueClusterFamilyPoolNoClusterTyped pins the dependent-site
// gate: no <cluster> typed yet means zero candidates and zero requests.
// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
func TestLiveFlagValueClusterFamilyPoolNoClusterTyped(t *testing.T) {
	srv, hits := clusterFamilyPoolServer(t)
	setClusterFamilyCompletionConfig(t, srv)

	site := CompletionSite{Kind: SiteFlagValue, FlagName: "pool", FlagSpelling: "--pool"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := liveCandidates(ctx, site, clusterFamilyPoolDescriptor, nil, []string{"--pool"})
	cancel()
	assert.Nil(t, got)
	assert.Zero(t, hits.Load())
}
