package complete

import (
	"testing"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const clientPath = "/k8s/clusters/local/apis/management.cattle.io/v3/oidcclients/my-app"

func clientServer(t *testing.T) *completiontest.Server {
	t.Helper()
	return completiontest.New(t, map[string]string{
		clientPath: `{"metadata":{"name":"my-app"},"status":{"clientSecrets":{"client-secret-1":{},"client-secret-2":{},"evil;rm -rf":{}}}}`,
	})
}

// TestSecretNameSourceClaim asserts the second positional is claimed once
// a client is typed, with a management need and one request, and the
// client positional, a later positional, a flag and a site without a
// typed client are declined.
func TestSecretNameSourceClaim(t *testing.T) {
	t.Parallel()
	src := SecretNameSource()
	reqs, ok := src.Claim(completion.Request{Index: 1, Positionals: []string{"my-app"}})
	require.True(t, ok)
	assert.Equal(t, completion.ManagementListing(), reqs)
	for _, req := range []completion.Request{
		{Index: 0},
		{Index: 1},
		{Index: 2, Positionals: []string{"my-app", "client-secret-1"}},
		{Flag: "timeout", Positionals: []string{"my-app"}},
	} {
		_, ok := src.Claim(req)
		assert.False(t, ok, "%+v", req)
	}
}

// TestCompleteSecretNames asserts one GET of the typed client answers its
// secret names, raw, for the surface to sanitize and sort; a client name
// that is not a path segment answers nothing without a request; an
// unknown client answers nothing.
func TestCompleteSecretNames(t *testing.T) {
	t.Parallel()
	s := clientServer(t)
	got := SecretNameSource().Complete(t.Context(), s.Conn(1), completion.Request{Index: 1, Positionals: []string{"my-app"}})
	assert.ElementsMatch(t, []string{"client-secret-1", "client-secret-2", "evil;rm -rf"}, got.Names)
	assert.Equal(t, []string{clientPath}, s.Requested())

	conn := clientServer(t).Conn(1)
	got = SecretNameSource().Complete(t.Context(), conn, completion.Request{Index: 1, Positionals: []string{"a/b"}})
	assert.Empty(t, got.Names)
	assert.Zero(t, conn.Used())

	conn = clientServer(t).Conn(1)
	got = SecretNameSource().Complete(t.Context(), conn, completion.Request{Index: 1, Positionals: []string{"nonesuch"}})
	assert.Empty(t, got.Names)
	assert.Equal(t, 1, conn.Used())
}
