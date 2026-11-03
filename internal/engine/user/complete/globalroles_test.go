package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGlobalRoleSourceClaimsTheFlagValueAlone asserts the source claims the
// --global-role value with a management need, no cluster need and one
// listing, and declines every other site.
func TestGlobalRoleSourceClaimsTheFlagValueAlone(t *testing.T) {
	t.Parallel()
	reqs, ok := GlobalRoleSource().Claim(completion.Request{Flag: "global-role"})
	require.True(t, ok)
	assert.Equal(t, completion.ManagementListing(), reqs)
	for _, req := range []completion.Request{
		{},
		{Index: 0},
		{Flag: "description"},
		{Flag: "role"},
		{Flag: "display-name"},
	} {
		_, ok := GlobalRoleSource().Claim(req)
		assert.False(t, ok, "%+v", req)
	}
}

// TestGlobalRoleSourceCompletes asserts one listing of the GlobalRoles
// answers every id, each described by its display name, a repeated row once,
// and a refused listing answering nothing.
func TestGlobalRoleSourceCompletes(t *testing.T) {
	t.Parallel()
	s := completiontest.NewFixed(t, `{"items":[
		{"metadata":{"name":"admin"},"displayName":"Administrator"},
		{"metadata":{"name":"user"},"displayName":"User","newUserDefault":true},
		{"metadata":{"name":"user"},"displayName":"User"},
		{"metadata":{"name":"user-base"}}
	]}`)
	got := GlobalRoleSource().Complete(t.Context(), s.Conn(1), completion.Request{Flag: "global-role"})
	assert.Equal(t, []string{"admin", "user", "user-base"}, got.Names)
	assert.Equal(t, map[string]string{"admin": "Administrator", "user": "User"}, got.Descriptions)
	assert.Equal(t, []string{"/k8s/clusters/local/apis/management.cattle.io/v3/globalroles"}, s.Requested())

	refused := completiontest.NewStatus(t, 403)
	assert.Empty(t, GlobalRoleSource().Complete(t.Context(), refused.Conn(1), completion.Request{Flag: "global-role"}).Names)
}
