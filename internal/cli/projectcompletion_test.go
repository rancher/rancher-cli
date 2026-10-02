package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectPagesServer serves the steve project collection of c-aaa11 in two
// pages and records every request URI.
func projectPagesServer(t *testing.T, status int) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var uris []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uris = append(uris, r.URL.RequestURI())
		mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		if r.URL.Path != "/v1/management.cattle.io.projects/c-aaa11" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("continue") == "" {
			_, _ = w.Write([]byte(`{"count":2,"revision":"1","continue":"n","data":[{"id":"c-aaa11/p-1","metadata":{"name":"p-1"},"spec":{"clusterName":"c-aaa11","displayName":"Default"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"count":2,"revision":"1","data":[{"id":"c-aaa11/p-2","metadata":{"name":"p-2"},"spec":{"clusterName":"c-aaa11","displayName":"System"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(uris)
	}
}

// TestListProjectsReadsTheSteveCollectionThroughTheConnection asserts the
// project lister the completion surface sets reads the cluster's steve
// collection page by page through the TAB's connection, so every page
// counts against its allowance, and that an unusable cluster id, a refused
// read and a spent allowance each answer an error.
func TestListProjectsReadsTheSteveCollectionThroughTheConnection(t *testing.T) {
	t.Parallel()
	srv, requested := projectPagesServer(t, http.StatusOK)
	conn := tracedConnection(srv.Client(), TokenCapability{Token: "tok", ServerURL: srv.URL}, 2)
	got, err := conn.Projects(t.Context(), "c-aaa11")
	require.NoError(t, err)
	assert.Equal(t, []completion.ProjectItem{{ID: "p-1", DisplayName: "Default"}, {ID: "p-2", DisplayName: "System"}}, got)
	assert.Equal(t, []string{"/v1/management.cattle.io.projects/c-aaa11", "/v1/management.cattle.io.projects/c-aaa11?continue=n"}, requested())

	_, err = conn.Projects(t.Context(), "c-aaa11")
	require.ErrorIs(t, err, completion.ErrAllowanceExceeded, "the two pages spent the allowance")

	spaced := tracedConnection(srv.Client(), TokenCapability{Token: "tok", ServerURL: srv.URL}, 2)
	_, err = spaced.Projects(t.Context(), "prod cluster")
	require.Error(t, err)
	assert.Len(t, requested(), 2, "an unusable cluster id costs no request")

	refused, _ := projectPagesServer(t, http.StatusForbidden)
	_, err = tracedConnection(refused.Client(), TokenCapability{Token: "tok", ServerURL: refused.URL}, 2).Projects(t.Context(), "c-aaa11")
	require.Error(t, err)
}

// TestCompletionGetterServesRootCollectionsOnly asserts the completion
// getter maps a non-2xx answer to steve's status error and refuses a
// request for one object or through a cluster's proxy without sending it.
func TestCompletionGetterServesRootCollectionsOnly(t *testing.T) {
	t.Parallel()
	srv, requested := projectPagesServer(t, http.StatusNotFound)
	get := completionGetter(completion.NewConnection(srv.Client(), "tok", srv.URL, false, 5))
	_, err := get(t.Context(), steve.Request{Type: steve.ProjectsType, Namespace: "c-aaa11"})
	se, ok := errors.AsType[*steve.StatusError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, http.StatusNotFound, se.Status)
	for _, r := range []steve.Request{{Cluster: "c-aaa11", Type: "namespaces"}, {Type: steve.ProjectsType, Namespace: "c-aaa11", Name: "p-1"}} {
		_, err := get(t.Context(), r)
		require.Error(t, err, "%+v", r)
	}
	assert.Len(t, requested(), 1)
}
