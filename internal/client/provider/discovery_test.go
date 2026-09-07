package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	return apidiscoveryv2.APIGroupDiscovery{ObjectMeta: metav1.ObjectMeta{Name: name}, Versions: []apidiscoveryv2.APIVersionDiscovery{v}}
}

// legacyDiscovery serves the pre-aggregation discovery documents client-go
// walks: /api, /api/v1, /apis, /apis/<group>/<version>. Group b.example
// answers 503 like an aggregated API whose backend is gone.
func legacyDiscovery(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/api":
		writeJSON(w, "application/json", metav1.APIVersions{TypeMeta: metav1.TypeMeta{Kind: "APIVersions"}, Versions: []string{"v1"}})
	case "/api/v1":
		writeJSON(w, "application/json", metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList"}, GroupVersion: "v1",
			APIResources: []metav1.APIResource{{Name: "pods", SingularName: "pod", Kind: "Pod", Namespaced: true, Verbs: []string{"get", "list"}}}})
	case "/apis":
		writeJSON(w, "application/json", metav1.APIGroupList{TypeMeta: metav1.TypeMeta{Kind: "APIGroupList"}, Groups: []metav1.APIGroup{
			{Name: "a.example", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "a.example/v1", Version: "v1"}}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "a.example/v1", Version: "v1"}},
			{Name: "b.example", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "b.example/v1", Version: "v1"}}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "b.example/v1", Version: "v1"}},
		}})
	case "/apis/a.example/v1":
		writeJSON(w, "application/json", metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList"}, GroupVersion: "a.example/v1",
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
			writeJSON(w, aggregatedContentType, apidiscoveryv2.APIGroupDiscoveryList{TypeMeta: metav1.TypeMeta{Kind: "APIGroupDiscoveryList"},
				Items: []apidiscoveryv2.APIGroupDiscovery{aggregatedGroup("", "v1", apidiscoveryv2.DiscoveryFreshnessCurrent, "pods")}})
		case "/apis":
			writeJSON(w, aggregatedContentType, apidiscoveryv2.APIGroupDiscoveryList{TypeMeta: metav1.TypeMeta{Kind: "APIGroupDiscoveryList"},
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
			writeJSON(w, "application/json", metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList"}, GroupVersion: "b.example/v1",
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
		writeJSON(w, aggregatedContentType, apidiscoveryv2.APIGroupDiscoveryList{TypeMeta: metav1.TypeMeta{Kind: "APIGroupDiscoveryList"}, Items: groups})
	}))
	defer srv.Close()
	c := discoveryClient(t, srv).(*dynamicClient)
	_, _, ok := c.aggregatedResources(t.Context())
	assert.False(t, ok, "a document past the cap must not pass as aggregated discovery")
}
