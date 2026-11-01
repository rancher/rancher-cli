package provisioning

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestPoolHintsPutDoubleDashBeforeADashedClusterName asserts the hints of the
// pool errors: the pause previews, and a name that starts with a dash follows
// the flags and "--".
func TestPoolHintsPutDoubleDashBeforeADashedClusterName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, clierr.Rancher+" cluster pause-autoscaling 'c1' --dry-run", autoscaledPoolGuardError("c1", "pool1").Hint)
	assert.Equal(t, clierr.Rancher+" cluster pause-autoscaling --dry-run -- '-c'", autoscaledPoolGuardError("-c", "pool1").Hint)
	assert.Equal(t, clierr.Rancher+" inspect cluster 'c1'", poolNotFoundError("c1", "pool1").Hint)
	assert.Equal(t, clierr.Rancher+" inspect cluster -- '-c'", poolNotFoundError("-c", "pool1").Hint)
}
