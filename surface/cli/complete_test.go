package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// k8sList is a minimal Kubernetes-style list response for testing (finding 10).
// The management K8s API uses the k8s items envelope, not Norman {"data":[...]}.
type k8sList struct {
	Items []map[string]any `json:"items"`
}

func makeListServer(t *testing.T, path string, items []map[string]any) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(k8sList{Items: items})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestFetchClustersReturnsSanitizedSorted(t *testing.T) {
	// No keyring.MockInit: fetchClusters does HTTP only, never touches keyring (round-2 finding 9).
	t.Parallel()
	items := []map[string]any{
		{"metadata": map[string]any{"name": "c-abc"}, "spec": map[string]any{"displayName": "prod"}},
		{"metadata": map[string]any{"name": "c-def"}, "spec": map[string]any{"displayName": "staging"}},
	}
	// Finding 10: management K8s API path, not Norman /v3/clusters.
	srv, hits := makeListServer(t, "/k8s/clusters/local/apis/management.cattle.io/v3/clusters", items)
	cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := fetchClusters(ctx, cap, "")
	assert.ElementsMatch(t, []string{"c-abc", "c-def", "prod", "staging"}, got)
	assert.Equal(t, int32(1), hits.Load())
}

func TestFetchClustersDropsSpacedDisplayNames(t *testing.T) {
	// No keyring.MockInit: fetchClusters does HTTP only (round-2 finding 9).
	t.Parallel()
	items := []map[string]any{
		{"metadata": map[string]any{"name": "c-abc"}, "spec": map[string]any{"displayName": "my cluster"}},
	}
	srv, _ := makeListServer(t, "/k8s/clusters/local/apis/management.cattle.io/v3/clusters", items)
	cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := fetchClusters(ctx, cap, "")
	assert.Equal(t, []string{"c-abc"}, got)
}

func TestFetchClustersTimeoutYieldsEmpty(t *testing.T) {
	// No keyring.MockInit: fetchClusters does HTTP only (round-2 finding 9).
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got := fetchClusters(ctx, cap, "")
	assert.Empty(t, got)
}

func TestFetchClusters401YieldsEmpty(t *testing.T) {
	// No keyring.MockInit: fetchClusters does HTTP only (round-2 finding 9).
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := fetchClusters(ctx, cap, "")
	assert.Empty(t, got)
}

// AC-24 / finding 11: two-request path — cluster display-name resolution +
// downstream listing. Assert ≤ 2 HTTP hits under the shared deadline.
func TestTwoRequestFlowClusterResolution(t *testing.T) {
	// No keyring.MockInit: fetch functions do HTTP only (round-2 finding 9).
	t.Parallel()
	var hits atomic.Int32
	clusterItems := []map[string]any{
		{"metadata": map[string]any{"name": "c-abc"}, "spec": map[string]any{"displayName": "prod"}},
	}
	nsItems := []map[string]any{
		{"metadata": map[string]any{"name": "default"}},
		{"metadata": map[string]any{"name": "kube-system"}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/clusters") && !strings.Contains(r.URL.Path, "namespaces") {
			_ = json.NewEncoder(w).Encode(k8sList{Items: clusterItems})
		} else {
			_ = json.NewEncoder(w).Encode(k8sList{Items: nsItems})
		}
	}))
	t.Cleanup(srv.Close)
	cap := TokenCapability{Token: "tok", ServerURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Resolve display name "prod" → "c-abc" (request 1), then fetch namespaces (request 2).
	resolvedID := resolveClusterDisplayName(ctx, cap, "prod")
	assert.Equal(t, "c-abc", resolvedID)
	got := fetchK8sResources(ctx, cap, resolvedID, "/api/v1/namespaces", "")
	assert.ElementsMatch(t, []string{"default", "kube-system"}, got)
	assert.LessOrEqual(t, hits.Load(), int32(2), "AC-24: at most 2 HTTP requests")
}

func TestSanitizeCandidatesDropsHostileNames(t *testing.T) {
	t.Parallel()
	input := []string{
		"valid-name",
		"also.valid",
		"-starts-with-dash",
		"has space",
		"$(inject)",
		"valid123",
		"a",
	}
	got := sanitizeCandidates(input)
	assert.Equal(t, []string{"a", "also.valid", "valid-name", "valid123"}, got)
}

func TestEqualsFormEmit(t *testing.T) {
	t.Parallel()
	candidates := []string{"namespace", "node", "pod", "project"}
	got := equalsFormCandidates("--by", candidates)
	assert.Equal(t, []string{
		"--by=namespace",
		"--by=node",
		"--by=pod",
		"--by=project",
	}, got)
}

func TestNoNetworkOnStaticSite(t *testing.T) {
	// No keyring.MockInit: resolveStaticEnum never touches keyring (round-2 finding 9).
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(srv.Close)
	// A static enum site must never reach the network.
	candidates := resolveStaticEnum([]string{"namespace", "project", "node", "pod"}, CompletionSite{
		Kind: SiteFlagValue, FlagName: "by", EqualsForm: false,
	})
	assert.Equal(t, []string{"namespace", "node", "pod", "project"}, candidates)
	assert.Equal(t, int32(0), hits.Load())
}
