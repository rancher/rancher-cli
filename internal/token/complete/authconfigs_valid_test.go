package complete

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
)

// TestEveryOfferedAuthConfigNamePassesTheNameCheckOfACallWithUserID asserts
// each name the source offers is an auth config's Kubernetes name, which is
// what a call with --user-id and --auth-provider requires of the value, so a
// suggestion is never refused as a VALIDATION error.
func TestEveryOfferedAuthConfigNamePassesTheNameCheckOfACallWithUserID(t *testing.T) {
	t.Parallel()
	srv := completiontest.New(t, map[string]string{"/v1-public/authproviders": `{"data":[
		{"id":"local","type":"localProvider"},
		{"id":"corp-ldap","type":"openLdapProvider"},
		{"id":"eu.ldap","type":"openLdapProvider"},
		{"id":"azuread","type":"azureADProvider"}]}`})
	conn := completion.NewConnection(srv.HTTP.Client(), "", srv.HTTP.URL, false, 1)
	got := AuthConfigSource().Complete(t.Context(), conn, completion.Request{Flag: "auth-provider"})
	require.NotEmpty(t, got.Names)
	for _, name := range got.Names {
		assert.Empty(t, validation.IsDNS1123Subdomain(name), name)
	}
}
