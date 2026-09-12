package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
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
	s := completiontest.NewFixed(t, `{"items":[
		{"metadata":{"name":"p-1"},"spec":{"displayName":"Default"}},
		{"metadata":{"name":"p-2"},"spec":{"displayName":"System"}},
		{"metadata":{"name":"p-3"},"spec":{"displayName":"System"}}
	]}`)
	got := ProjectSource().Complete(t.Context(), s.Conn(1), completion.Request{Flag: "project", Cluster: "prod", ClusterID: "c-aaa11"})
	assert.Equal(t, []string{"p-1", "Default", "p-2", "p-3"}, got.Names)
	assert.Equal(t, []string{"/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/c-aaa11/projects"}, s.Requested())

	got = ProjectSource().Complete(t.Context(), s.Conn(1), completion.Request{ClusterID: "prod cluster"})
	assert.Empty(t, got.Names)
	assert.Len(t, s.Requested(), 1, "an unusable cluster id costs no request")

	refused := completiontest.NewStatus(t, 403)
	assert.Empty(t, ProjectSource().Complete(t.Context(), refused.Conn(1), completion.Request{ClusterID: "c-aaa11"}).Names)
}
