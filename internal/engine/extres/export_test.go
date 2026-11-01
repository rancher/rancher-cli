package extres

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// KubeconfigRefusalMessage is the message of the refusal of create kubeconfig
// on a server below 2.15 for a stored login named login, for the external
// tests of this package.
func KubeconfigRefusalMessage(t *testing.T, login string) string {
	t.Helper()
	var asks int
	clients := versionFactory{c: &fakeClient{}, version: "v2.14.5", login: login, versionAsks: &asks}
	ce, ok := refuseUnsupportedTokenLookup(t.Context(), clients, lookupForbidden(t)).(*clierr.Error)
	require.True(t, ok, "the lookup failure of %q is not refused", login)
	return ce.Message
}
