package complete

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectSourceClaim asserts the source claims any site with the
// typed cluster resolved, a management token and one listing.
func TestProjectSourceClaim(t *testing.T) {
	t.Parallel()
	for _, req := range []completion.Request{{Flag: "project"}, {Index: 0}} {
		reqs, ok := ProjectSource().Claim(req)
		require.True(t, ok)
		assert.Equal(t, completion.ClusterListing(), reqs)
	}
}

// TestProjectSourceCompletes asserts the Projects of the resolved
// cluster's namespace are read once and projected to ids plus unique
// display names, and that an unusable cluster id or a refused listing
// answers nothing.
func TestProjectSourceCompletes(t *testing.T) {
	t.Parallel()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"p-1"},"spec":{"displayName":"Default"}},
			{"metadata":{"name":"p-2"},"spec":{"displayName":"System"}},
			{"metadata":{"name":"p-3"},"spec":{"displayName":"System"}}
		]}`))
	}))
	t.Cleanup(srv.Close)
	conn := completion.NewConnection(srv.Client(), "tok", srv.URL, false, 1)
	got := ProjectSource().Complete(t.Context(), conn, completion.Request{Flag: "project", Cluster: "prod", ClusterID: "c-aaa11"})
	assert.Equal(t, []string{"p-1", "Default", "p-2", "p-3"}, got.Names)
	assert.Equal(t, []string{"/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-aaa11/projects"}, paths)

	paths = nil
	got = ProjectSource().Complete(t.Context(), completion.NewConnection(srv.Client(), "tok", srv.URL, false, 1), completion.Request{ClusterID: "prod cluster"})
	assert.Empty(t, got.Names)
	assert.Empty(t, paths)

	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	t.Cleanup(refused.Close)
	assert.Empty(t, ProjectSource().Complete(t.Context(), completion.NewConnection(refused.Client(), "tok", refused.URL, false, 1), completion.Request{ClusterID: "c-aaa11"}).Names)
}
