package projects

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestCreateProjectBuildRefusesAMissingCluster asserts a direct call of build
// with no --cluster, which bypasses the pipeline (it refuses a missing
// required flag first), is refused as VALIDATION with the text of a command
// that takes no namespace and the hint that lists the clusters.
func TestCreateProjectBuildRefusesAMissingCluster(t *testing.T) {
	t.Parallel()
	_, _, err := createCmd{}.build(in{a: []string{"web"}})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "error: %v", err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	assert.Equal(t, "--cluster is required; pass a cluster id or name", ce.Message)
	assert.Equal(t, clierr.Rancher+" access clusters", ce.Hint)
}
