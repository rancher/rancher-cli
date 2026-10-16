package complete

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
	"github.com/rancher/rancher-cli/internal/runtime/completion/completiontest"
)

// TestAuthConfigSourceClaim asserts the source claims the --auth-provider
// value site only, needing no credential and one request.
func TestAuthConfigSourceClaim(t *testing.T) {
	t.Parallel()
	reqs, ok := AuthConfigSource().Claim(completion.Request{Flag: "auth-provider"})
	require.True(t, ok)
	assert.Equal(t, completion.PublicListing(), reqs)
	assert.Equal(t, completion.CredentialNone, reqs.Credential)
	for _, req := range []completion.Request{{Flag: "cluster"}, {Index: 0}, {Flag: "name"}} {
		_, ok := AuthConfigSource().Claim(req)
		assert.False(t, ok, "%+v", req)
	}
}

// TestAuthConfigSourceOffersTheNamesOfSupportedConfigs asserts the names
// offered are the ids of the entries of a type this CLI signs in with, two
// configs of one type included, and not those of a type it drops or an
// entry with no id; and that it reads the provider list once.
func TestAuthConfigSourceOffersTheNamesOfSupportedConfigs(t *testing.T) {
	t.Parallel()
	srv := completiontest.New(t, map[string]string{"/v1-public/authproviders": `{"data":[
		{"id":"local","type":"localProvider"},
		{"id":"corp-ldap","type":"openLdapProvider"},
		{"id":"eu-ldap","type":"openLdapProvider"},
		{"id":"github","type":"githubProvider"},
		{"type":"oktaProvider"},
		{"id":"okta","type":"oktaProvider"}]}`})
	conn := completion.NewConnection(srv.HTTP.Client(), "", srv.HTTP.URL, false, 1)
	got := AuthConfigSource().Complete(t.Context(), conn, completion.Request{Flag: "auth-provider"})
	assert.Equal(t, []string{"local", "corp-ldap", "eu-ldap", "okta"}, got.Names)
	assert.Equal(t, []string{"/v1-public/authproviders"}, srv.Requested())
}

// TestAuthConfigSourceOffersNothingOnAFailure asserts a refused request or a
// body that is not a provider list answers no names.
func TestAuthConfigSourceOffersNothingOnAFailure(t *testing.T) {
	t.Parallel()
	for name, srv := range map[string]*completiontest.Server{
		"a refusal":  completiontest.NewStatus(t, 500),
		"not a list": completiontest.NewFixed(t, `[1,2]`),
	} {
		conn := completion.NewConnection(srv.HTTP.Client(), "", srv.HTTP.URL, false, 1)
		assert.Empty(t, AuthConfigSource().Complete(t.Context(), conn, completion.Request{Flag: "auth-provider"}).Names, name)
	}
}
