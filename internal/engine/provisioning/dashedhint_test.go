package provisioning

import (
	"strings"
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
	assert.Contains(t, autoscaledPoolGuardError("c1", "pool1").Message, "; the hint previews it: add --apply --yes after reading the preview")
	assert.Equal(t, clierr.Rancher+" inspect cluster 'c1'", poolNotFoundError("c1", "pool1").Hint)
	assert.Equal(t, clierr.Rancher+" inspect cluster -- '-c'", poolNotFoundError("-c", "pool1").Hint)
}

// TestPoolHintsAreLeftOutForAClusterNameThatCannotBePrinted asserts a cluster
// name the shell would misread leaves the command out of the hint and puts the
// left-out note in the message once, with the cluster named in prose.
func TestPoolHintsAreLeftOutForAClusterNameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for name, cluster := range map[string]string{
		"apostrophe":       "c'1",
		"curly apostrophe": "c\u20191",
		"zero width space": "c\u200b1",
		"double backslash": "c\\\\1",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for what, ce := range map[string]*clierr.Error{
				"pool not found": poolNotFoundError(cluster, "pool1"),
				"autoscaled":     autoscaledPoolGuardError(cluster, "pool1"),
			} {
				assert.Empty(t, ce.Hint, what)
				assert.Equal(t, 1, strings.Count(ce.Message, clierr.LeftOutNote), what)
				assert.NotContains(t, ce.Message, "previews it", what)
				assert.NotContains(t, ce.Message, "--apply", what)
			}
		})
	}
}
