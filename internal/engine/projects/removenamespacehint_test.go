package projects

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestRemoveNamespaceHintPreviewsTheRemoval asserts the preview of removing a
// namespace from its project, for an ordinary name and for a name that starts
// with a dash.
func TestRemoveNamespaceHintPreviewsTheRemoval(t *testing.T) {
	t.Parallel()
	assert.Equal(t, clierr.Rancher+" project remove-namespace 'team-a' --cluster 'c-x' --dry-run", removeNamespaceHint("team-a", "c-x"))
	assert.Equal(t, clierr.Rancher+" project remove-namespace --cluster 'c-x' --dry-run -- '-x'", removeNamespaceHint("-x", "c-x"))
}
