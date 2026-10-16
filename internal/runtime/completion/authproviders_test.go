package completion

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuthProvidersPath asserts the builder gives the server's
// unauthenticated provider list, at the server root.
func TestAuthProvidersPath(t *testing.T) {
	t.Parallel()
	p, err := AuthProvidersPath()
	require.NoError(t, err)
	assert.Equal(t, "/v1-public/authproviders", p.String())
}

// TestPublicListingNeedsNoCredential asserts the declaration of a source
// that reads an unauthenticated route: no cluster, no credential, one
// request. CredentialNone is a new member, so the zero value of the need
// stays CredentialAny.
func TestPublicListingNeedsNoCredential(t *testing.T) {
	t.Parallel()
	assert.Equal(t, Requirements{Cluster: ClusterNone, Credential: CredentialNone, Allowance: AllowanceOne}, PublicListing())
	assert.NotEqual(t, CredentialAny, CredentialNone)
	assert.NotEqual(t, CredentialManagement, CredentialNone)
	assert.Equal(t, CredentialNeed(0), CredentialAny, "the zero value keeps its meaning for every existing source")
}
