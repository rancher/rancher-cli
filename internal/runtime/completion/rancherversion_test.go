package completion

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRancherVersionPath asserts the builder gives the server's version
// document at the server root and takes no segment.
func TestRancherVersionPath(t *testing.T) {
	t.Parallel()
	p, err := RancherVersionPath()
	require.NoError(t, err)
	assert.Equal(t, "/rancherversion", p.String())
}

// TestManagementTwoListingsReadsTheVersionDocumentAndAList asserts the
// declaration of a source that reads the version document and then a
// collection: no cluster, a management token, two requests.
func TestManagementTwoListingsReadsTheVersionDocumentAndAList(t *testing.T) {
	t.Parallel()
	assert.Equal(t, Requirements{Cluster: ClusterNone, Credential: CredentialManagement, Allowance: AllowanceTwo}, ManagementTwoListings())
}
