package completion

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReleaseListPath asserts the builder gives the release list route of
// each of the two distributions and refuses any other distribution.
func TestReleaseListPath(t *testing.T) {
	t.Parallel()
	for distro, want := range map[string]string{"rke2": "/v1-rke2-release/release", "k3s": "/v1-k3s-release/release"} {
		p, err := ReleaseListPath(distro)
		require.NoError(t, err, distro)
		assert.Equal(t, want, p.String())
	}
	for _, distro := range []string{"", "RKE2", "k3os", "rke2/../x", "../k3s", "rke2 "} {
		_, err := ReleaseListPath(distro)
		assert.ErrorIs(t, err, ErrInvalidSegment, "%q", distro)
	}
}
