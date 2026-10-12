package token

import (
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeleteTokenDeletesAnUnscopedV3TokenAndNamesTheAdministratorForAScopedOne
// asserts auth delete-token deletes an unscoped v3 cache token with its own
// bearer and without a warning, and that a cluster-scoped v3 cache token is
// reported v3, manual, with the administrator's delete in the data and the
// warning, and stays on the server.
func TestDeleteTokenDeletesAnUnscopedV3TokenAndNamesTheAdministratorForAScopedOne(t *testing.T) {
	x := newRevFixture(t, v3Options)
	x.login("prod", "alice")
	unscoped, scoped := x.v3Token("alice"), x.v3Token("alice")
	x.cacheOn("unscoped", unscoped, "", x.own("prod"))
	x.cacheOn("scoped", scoped, "c-1", x.own("prod"))

	res, err := NewDeleteTokenCommand(x.dir, x.store).(runtime.ReadCommand).Execute(t.Context(), tin{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	data, _ := decodeDeleteToken(t, res)
	assert.Equal(t, []string{unscoped.Ref.Name}, revokedNames(RevocationReport{Revoked: data.Revoked}))
	require.Len(t, data.NotRevoked, 1)
	n := data.NotRevoked[0]
	assert.Equal(t, scoped.Ref.Name, n.Ref.Name)
	assert.Equal(t, "v3", n.Reason)
	assert.Equal(t, "manual", n.Retry)
	assert.Equal(t, []string{v3AdminDeleteCommand(scoped.Ref.Name)}, n.Recovery)
	require.Len(t, res.Warnings, 1)
	assert.True(t, strings.HasPrefix(res.Warnings[0], "the cluster token "+scoped.Ref.String()+" was not revoked: the CLI cannot delete this v3 token; an administrator can delete it with: "), res.Warnings[0])
	assert.False(t, x.alive(unscoped))
	assert.True(t, x.alive(scoped))
	logouts, deletes := x.requestsFor(unscoped.Ref.Name)
	require.Len(t, logouts, 1)
	assert.Equal(t, unscoped.Token, logouts[0].Bearer)
	assert.Empty(t, deletes)
	logouts, deletes = x.requestsFor(scoped.Ref.Name)
	assert.Empty(t, logouts)
	assert.Empty(t, deletes)
}
