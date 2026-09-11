package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/observe"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
)

// logsPodServer serves a namespaced pod list under the management K8s proxy,
// counting hits and recording the last labelSelector query it received.
func logsPodServer(t *testing.T) (srv *httptest.Server, hits *atomic.Int32, lastSelector *string) {
	t.Helper()
	hits = &atomic.Int32{}
	lastSelector = new(string)
	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		*lastSelector = r.URL.Query().Get("labelSelector")
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"rancher-webhook-65fcbccf9f-1"}},
			{"metadata":{"name":"rancher-webhook-65fcbccf9f-2"}}
		]}`))
	}
	mux.HandleFunc("/k8s/clusters/local/api/v1/namespaces/cattle-system/pods", handler)
	mux.HandleFunc("/k8s/clusters/c-1/api/v1/namespaces/cattle-system/pods", handler)
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/clusters",
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"items":[
				{"metadata":{"name":"c-1"},"spec":{"displayName":"prod"}}
			]}`))
		})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, hits, lastSelector
}

var logsDescriptor = runtime.Descriptor{
	Name: "logs",
	Args: []runtime.ArgSpec{{Name: "target", Type: "string", Required: true, Enum: observe.Names()}},
	Flags: []runtime.ArgSpec{
		{Name: "cluster", Type: "string"},
		{Name: "tail", Type: "string"},
		{Name: "since", Type: "string"},
		{Name: "previous", Type: "bool"},
		{Name: "container", Type: "string"},
		{Name: "timestamps", Type: "bool"},
		{Name: "pod", Type: "string"},
	},
}

func TestLogsPodCandidatesLocalOnlyTargetNeedsNoCluster(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	srv, hits, lastSelector := logsPodServer(t)
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	site := CompletionSite{Kind: SiteFlagValue, FlagName: "pod", FlagSpelling: "--pod"}
	tail := []string{"webhook", "--pod"}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := liveCandidates(ctx, site, logsDescriptor, nil, tail)
	assert.Equal(t, []string{"rancher-webhook-65fcbccf9f-1", "rancher-webhook-65fcbccf9f-2"}, got)
	assert.Equal(t, int32(1), hits.Load(), "webhook is LocalOnly: one request, no --cluster needed")
	assert.Equal(t, "app=rancher-webhook", *lastSelector)
}

func TestLogsPodCandidatesDownstreamOnlyTargetNeedsTypedCluster(t *testing.T) {
	// NOT parallel: mutates storedConfig and RANCHER_CLI_TOKEN env globals.
	srv, hits, lastSelector := logsPodServer(t)
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())
	t.Setenv(config.EnvToken, "mgmt-token")

	site := CompletionSite{Kind: SiteFlagValue, FlagName: "pod", FlagSpelling: "--pod"}

	t.Run("cluster typed on the tail", func(t *testing.T) {
		tail := []string{"agent", "--cluster", "c-1", "--pod"}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := liveCandidates(ctx, site, logsDescriptor, nil, tail)
		assert.Equal(t, []string{"rancher-webhook-65fcbccf9f-1", "rancher-webhook-65fcbccf9f-2"}, got)
		assert.Equal(t, "app=cattle-cluster-agent", *lastSelector)
	})

	t.Run("display name resolves to the cluster id", func(t *testing.T) {
		// --cluster prod is the DISPLAY name of c-1: completion must
		// resolve it exactly like the generic pipeline instead of
		// building a /k8s/clusters/prod/... URL that 404s into silence.
		tail := []string{"agent", "--cluster", "prod", "--pod"}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := liveCandidates(ctx, site, logsDescriptor, nil, tail)
		assert.Equal(t, []string{"rancher-webhook-65fcbccf9f-1", "rancher-webhook-65fcbccf9f-2"}, got)
	})

	t.Run("no --cluster typed yields no candidates, zero requests", func(t *testing.T) {
		hits.Store(0)
		tail := []string{"agent", "--pod"}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		got := liveCandidates(ctx, site, logsDescriptor, nil, tail)
		assert.Nil(t, got)
		assert.Zero(t, hits.Load())
	})
}

func TestLogsPodCandidatesFleetAgentHasNoStaticNamespace(t *testing.T) {
	t.Parallel()
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "pod", FlagSpelling: "--pod"}
	tail := []string{"fleet-agent", "--pod"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := liveCandidates(ctx, site, logsDescriptor, nil, tail)
	assert.Nil(t, got, "fleet-agent's namespace is resolved dynamically at Execute time, not from the static table")
}

func TestLogsPodCandidatesUnknownOrIncompleteTargetYieldsNil(t *testing.T) {
	t.Parallel()
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "pod", FlagSpelling: "--pod"}
	tail := []string{"--pod"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := liveCandidates(ctx, site, logsDescriptor, nil, tail)
	assert.Nil(t, got)
}

// TestLogsTargetCompletionYieldsExactlyTheTargetTable asserts `logs <TAB>`
// against the curated target table: the positional's Enum (sourced from
// observe.Names(), never a hand-copied list) must offer exactly those five
// names, sorted, and nothing else.
func TestLogsTargetCompletionYieldsExactlyTheTargetTable(t *testing.T) {
	t.Parallel()
	got, owned := positionalEnumCandidates(CompletionSite{Kind: SitePositional, Index: 0}, logsDescriptor)
	assert.True(t, owned)
	assert.Equal(t, []string{"agent", "api-extension", "fleet-agent", "kube-api-auth", "server", "webhook"}, got)
}
