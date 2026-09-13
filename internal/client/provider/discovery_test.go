package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apidiscoveryv2 "k8s.io/api/apidiscovery/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/rancher/rancher-cli/internal/engine"
)

const aggregatedContentType = "application/json;g=apidiscovery.k8s.io;v=v2;as=APIGroupDiscoveryList"

func writeJSON(w http.ResponseWriter, contentType string, v any) {
	w.Header().Set("Content-Type", contentType)
	_ = json.NewEncoder(w).Encode(v)
}

func aggregatedGroup(name, version string, fresh apidiscoveryv2.DiscoveryFreshness, resources ...string) apidiscoveryv2.APIGroupDiscovery {
	v := apidiscoveryv2.APIVersionDiscovery{Version: version, Freshness: fresh}
	for _, r := range resources {
		v.Resources = append(v.Resources, apidiscoveryv2.APIResourceDiscovery{Resource: r, Scope: apidiscoveryv2.ScopeNamespace, Verbs: []string{"get", "list"}})
	}
	return apidiscoveryv2.APIGroupDiscovery{Name: name, Versions: []apidiscoveryv2.APIVersionDiscovery{v}}
}

// legacyDiscovery serves the pre-aggregation discovery documents client-go
// walks: /api, /api/v1, /apis, /apis/<group>/<version>. Group b.example
// answers 503 like an aggregated API whose backend is gone.
func legacyDiscovery(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/api":
		writeJSON(w, "application/json", metav1.APIVersions{Kind: "APIVersions", Versions: []string{"v1"}})
	case "/api/v1":
		writeJSON(w, "application/json", metav1.APIResourceList{Kind: "APIResourceList", GroupVersion: "v1",
			APIResources: []metav1.APIResource{{Name: "pods", SingularName: "pod", Kind: "Pod", Namespaced: true, Verbs: []string{"get", "list"}}}})
	case "/apis":
		writeJSON(w, "application/json", metav1.APIGroupList{Kind: "APIGroupList", Groups: []metav1.APIGroup{
			{Name: "a.example", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "a.example/v1", Version: "v1"}}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "a.example/v1", Version: "v1"}},
			{Name: "b.example", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "b.example/v1", Version: "v1"}}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "b.example/v1", Version: "v1"}},
		}})
	case "/apis/a.example/v1":
		writeJSON(w, "application/json", metav1.APIResourceList{Kind: "APIResourceList", GroupVersion: "a.example/v1",
			APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: []string{"get"}}}})
	case "/apis/b.example/v1":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("service unavailable\n"))
	default:
		return false
	}
	return true
}

func discoveryClient(t *testing.T, srv *httptest.Server) engine.ClusterClient {
	t.Helper()
	c, err := NewClusterClient(&rest.Config{Host: srv.URL, BearerToken: "t"}, "local")
	require.NoError(t, err)
	return c
}

// TestServerResourcesAggregatedMarksStaleGroup asserts the fast path: two
// requests, both roots aggregated, a stale group version reported as
// DiscoveryIncomplete alongside the resources the server could describe.
func TestServerResourcesAggregatedMarksStaleGroup(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	var accepts atomic.Value // the last Accept header seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		accepts.Store(r.Header.Get("Accept"))
		switch r.URL.Path {
		case "/api":
			writeJSON(w, aggregatedContentType, apidiscoveryv2.APIGroupDiscoveryList{Kind: "APIGroupDiscoveryList",
				Items: []apidiscoveryv2.APIGroupDiscovery{aggregatedGroup("", "v1", apidiscoveryv2.DiscoveryFreshnessCurrent, "pods")}})
		case "/apis":
			writeJSON(w, aggregatedContentType, apidiscoveryv2.APIGroupDiscoveryList{Kind: "APIGroupDiscoveryList",
				Items: []apidiscoveryv2.APIGroupDiscovery{
					aggregatedGroup("a.example", "v1", apidiscoveryv2.DiscoveryFreshnessCurrent, "widgets"),
					aggregatedGroup("dead.example", "v1beta1", apidiscoveryv2.DiscoveryFreshnessStale),
				}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	res, err := discoveryClient(t, srv).ServerResources(t.Context())
	inc, ok := errors.AsType[*engine.DiscoveryIncomplete](err)
	require.True(t, ok, "a stale group is reported as DiscoveryIncomplete, got %v", err)
	assert.Equal(t, []string{"dead.example"}, inc.Groups())
	names := make([]string, 0, len(res))
	for _, r := range res {
		names = append(names, r.Group+"/"+r.Name)
	}
	assert.Equal(t, []string{"/pods", "a.example/widgets"}, names)
	assert.Equal(t, int32(2), requests.Load(), "aggregated discovery is two requests")
	assert.Contains(t, accepts.Load(), "as=APIGroupDiscoveryList", "the aggregated document is asked for by content type")
}

// TestServerResourcesFallsBackToLegacyAndKeepsPartialError asserts the
// Rancher-local shape: /apis answers a legacy APIGroupList whatever the
// Accept header says, so discovery takes the per-group path, and a group
// whose document answers 503 is reported as DiscoveryIncomplete alongside
// the resources of the groups that answered.
func TestServerResourcesFallsBackToLegacyAndKeepsPartialError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !legacyDiscovery(w, r) {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	res, err := discoveryClient(t, srv).ServerResources(t.Context())
	inc, ok := errors.AsType[*engine.DiscoveryIncomplete](err)
	require.True(t, ok, "a 503 group is reported as DiscoveryIncomplete, got %v", err)
	assert.Equal(t, []string{"b.example"}, inc.Groups())
	assert.Contains(t, inc.Failed, schema.GroupVersion{Group: "b.example", Version: "v1"})
	names := make([]string, 0, len(res))
	for _, r := range res {
		names = append(names, r.Group+"/"+r.Name)
	}
	assert.ElementsMatch(t, []string{"/pods", "a.example/widgets"}, names)
}

// TestServerResourcesEveryGroupFailedIsStillIncomplete asserts the
// contract holds when no group document answered: an empty list together
// with DiscoveryIncomplete naming every failed group version, never the
// raw client-go error.
func TestServerResourcesEveryGroupFailedIsStillIncomplete(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1", "/apis/a.example/v1", "/apis/b.example/v1":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("service unavailable\n"))
		default:
			if !legacyDiscovery(w, r) {
				http.NotFound(w, r)
			}
		}
	}))
	defer srv.Close()
	res, err := discoveryClient(t, srv).ServerResources(t.Context())
	inc, ok := errors.AsType[*engine.DiscoveryIncomplete](err)
	require.True(t, ok, "every group failing is still a partial answer, got %v", err)
	assert.Empty(t, res)
	assert.Equal(t, []string{"a.example", "b.example", "core"}, inc.Groups())
}

// TestServerResourcesCompleteAnswerHasNoError asserts a fully healthy legacy
// answer carries no DiscoveryIncomplete.
func TestServerResourcesCompleteAnswerHasNoError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apis/b.example/v1" {
			writeJSON(w, "application/json", metav1.APIResourceList{Kind: "APIResourceList", GroupVersion: "b.example/v1",
				APIResources: []metav1.APIResource{{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: []string{"get"}}}})
			return
		}
		if !legacyDiscovery(w, r) {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	res, err := discoveryClient(t, srv).ServerResources(t.Context())
	require.NoError(t, err)
	assert.Len(t, res, 3)
}

// TestServerResourcesPreCancelledMakesNoRequest asserts a cancelled context
// is honored before any request, on either path.
func TestServerResourcesPreCancelledMakesNoRequest(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if !legacyDiscovery(w, r) {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := discoveryClient(t, srv).ServerResources(ctx)
	require.Error(t, err)
	assert.Equal(t, int32(0), requests.Load(), "a pre-cancelled call sends nothing")
}

// TestServerResourcesCancelledMidFlightDoesNotFallBack asserts that a
// context cancelled while the aggregated request is in flight ends the
// call with the cancellation, never with a legacy walk of every group.
func TestServerResourcesCancelledMidFlightDoesNotFallBack(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		cancel()
		<-r.Context().Done()
	}))
	defer srv.Close()
	_, err := discoveryClient(t, srv).ServerResources(ctx)
	require.Error(t, err)
	assert.Equal(t, int32(1), requests.Load(), "no legacy walk after the cancellation")
	assert.NotContains(t, err.Error(), "b.example", "the error is the cancellation, not a discovery result")
}

// TestAggregatedResourcesRejectsOversizedDocument asserts a document
// larger than the cap is not read as a (truncated) complete inventory:
// the aggregated attempt reports not-ok and the caller falls back. Not
// parallel: it lowers the package-level cap.
func TestAggregatedResourcesRejectsOversizedDocument(t *testing.T) {
	old := maxDiscoveryBytes
	maxDiscoveryBytes = 256
	t.Cleanup(func() { maxDiscoveryBytes = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		groups := make([]apidiscoveryv2.APIGroupDiscovery, 0, 8)
		for i := range 8 {
			groups = append(groups, aggregatedGroup("group"+string(rune('a'+i))+".example", "v1", apidiscoveryv2.DiscoveryFreshnessCurrent, "widgets"))
		}
		writeJSON(w, aggregatedContentType, apidiscoveryv2.APIGroupDiscoveryList{Kind: "APIGroupDiscoveryList", Items: groups})
	}))
	defer srv.Close()
	c := discoveryClient(t, srv).(*dynamicClient)
	_, _, ok := c.aggregatedResources(t.Context())
	assert.False(t, ok, "a document past the cap must not pass as aggregated discovery")
}

// TestServerResourcesLegacyKeepsSubresourcesAndPreferredVersion asserts the
// legacy walk keeps every "<resource>/<sub>" entry (the commands resolve
// --subresource from them) and reads one document per group, the
// preferred version's, even when the group lists another version.
func TestServerResourcesLegacyKeepsSubresourcesAndPreferredVersion(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/api":
			writeJSON(w, "application/json", metav1.APIVersions{Kind: "APIVersions", Versions: []string{"v1"}})
		case "/api/v1":
			writeJSON(w, "application/json", metav1.APIResourceList{Kind: "APIResourceList", GroupVersion: "v1",
				APIResources: []metav1.APIResource{{Name: "pods", SingularName: "pod", Kind: "Pod", Namespaced: true}, {Name: "pods/log", Kind: "Pod", Namespaced: true}}})
		case "/apis":
			writeJSON(w, "application/json", metav1.APIGroupList{Kind: "APIGroupList", Groups: []metav1.APIGroup{
				{Name: "m.example", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "m.example/v2", Version: "v2"}, {GroupVersion: "m.example/v1", Version: "v1"}},
					PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "m.example/v1", Version: "v1"}},
			}})
		case "/apis/m.example/v1":
			writeJSON(w, "application/json", metav1.APIResourceList{Kind: "APIResourceList", GroupVersion: "m.example/v1",
				APIResources: []metav1.APIResource{{Name: "gauges", Kind: "Gauge", Namespaced: true}, {Name: "gauges/status", Kind: "Gauge", Namespaced: true}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	res, err := discoveryClient(t, srv).ServerResources(t.Context())
	require.NoError(t, err)
	names := make([]string, 0, len(res))
	for _, r := range res {
		names = append(names, r.Group+"/"+r.Name)
	}
	assert.Equal(t, []string{"/pods", "/pods/log", "m.example/gauges", "m.example/gauges/status"}, names)
	mu.Lock()
	defer mu.Unlock()
	assert.ElementsMatch(t, []string{"/api", "/api", "/apis", "/api/v1", "/apis/m.example/v1"}, paths,
		"the aggregated attempt (stopped at the first plain root), the two plain roots, one document per group in its preferred version - never v2")
}

// TestServerResourcesLegacyWalkEndsWithItsContext asserts a context
// cancelled while the group documents are being read ends the walk with
// the cancellation, not with a partial inventory.
func TestServerResourcesLegacyWalkEndsWithItsContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apis/a.example/v1" {
			cancel()
			<-r.Context().Done()
			return
		}
		if !legacyDiscovery(w, r) {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	res, err := discoveryClient(t, srv).ServerResources(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, res)
}
