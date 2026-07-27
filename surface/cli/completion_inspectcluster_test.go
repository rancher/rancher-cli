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

// managementClusterServer serves the management cluster list at the exact
// path fetchClusters reads, counting hits so zero-request contracts are
// provable.
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

// TestInspectClusterPositionalCompletesLive is IC-3's exception: the
// inspect cluster leaf completes live management-cluster names/ids via
// the same fetch get cluster's positional uses.
func TestInspectClusterPositionalCompletesLive(t *testing.T) {
	t.Parallel()
	srv, hits := managementClusterServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "inspect cluster"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
	assert.Contains(t, got, "c-abc")
	assert.Contains(t, got, "prod", "display names complete alongside ids (fetchClusters semantics)")
	assert.Equal(t, int32(1), hits.Load(), "exactly one cluster-list request")
}

// TestInspectClusterPositionalIDOnlyBoundary pins the IDOnly boundary as
// it applies to the cluster fetch (IC-16): an exec-cred-only capability
// cannot authorize the management cluster list — nil, zero requests.
func TestInspectClusterPositionalIDOnlyBoundary(t *testing.T) {
	t.Parallel()
	srv, hits := managementClusterServer(t)
	cap := TokenCapability{Token: "kubeconfig-user:secret", ServerURL: srv.URL, IDOnly: true}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "inspect cluster"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
	assert.Nil(t, got)
	assert.Zero(t, hits.Load(), "IDOnly: zero requests")
}

// TestOtherInspectLeavesRetainPositionalDeny pins the retained deny:
// the exceptions are exactly `inspect cluster` and `inspect project` —
// pod and deployment stay excluded with zero requests.
func TestOtherInspectLeavesRetainPositionalDeny(t *testing.T) {
	t.Parallel()
	srv, hits := managementClusterServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for _, name := range []string{"inspect pod", "inspect deployment"} {
		got := livePositionalCandidates(ctx, srv.Client(), site, runtime.Descriptor{Name: name}, cap, "c-abc", "", nil)
		assert.Nil(t, got, "%s must retain the live-positional deny", name)
	}
	assert.Zero(t, hits.Load(), "the denied leaves must issue zero requests")
}

// TestInspectProjectPositionalCompletes pins the second exception: the
// `inspect project` name positional completes from the management project
// list of the resolved cluster (members-ergonomics slice 4).
func TestInspectProjectPositionalCompletes(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-abc/projects" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"p-cvrp4"},"spec":{"displayName":"Default"}},
			{"metadata":{"name":"p-xyz12"},"spec":{"displayName":"System"}},
			{"metadata":{"name":"p-dup1"},"spec":{"displayName":"Twin"}},
			{"metadata":{"name":"p-dup2"},"spec":{"displayName":"Twin"}}
		]}`))
	}))
	defer srv.Close()
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "inspect project"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "c-abc", "", nil)
	// ids AND uniquely-resolving display names are candidates — the
	// user-visible point of the feature is completing project NAMES.
	assert.Contains(t, got, "p-cvrp4")
	assert.Contains(t, got, "p-xyz12")
	assert.Contains(t, got, "Default")
	assert.Contains(t, got, "System")
	// projectCandidates' validity filter must survive the new exception:
	// an ambiguous display name (two projects named Twin) is never offered —
	// a suggestion must resolve verbatim.
	assert.NotContains(t, got, "Twin")
	assert.Contains(t, got, "p-dup1")
	assert.Contains(t, got, "p-dup2")
}

// TestInspectClusterPositionalGateOff proves the network gate governs the
// exception exactly as it governs every live site: completion.network
// false → nil candidates, zero requests, before any dispatch.
func TestInspectClusterPositionalGateOff(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	srv, hits := managementClusterServer(t)
	cfg := &config.Config{
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv"}},
		Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:     config.CompletionConfig{Network: false},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "inspect cluster"}
	got := dispatchSite(context.Background(), site, d, nil, nil, nil)
	assert.Nil(t, got)
	assert.Zero(t, hits.Load(), "gate off: zero requests")
}
