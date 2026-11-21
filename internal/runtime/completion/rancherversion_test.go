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
