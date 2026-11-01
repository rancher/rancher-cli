package resources

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestDeleteProjectHintPreviewsTheDelete asserts the preview of deleting a
// project, for an ordinary id and for an id that starts with a dash.
func TestDeleteProjectHintPreviewsTheDelete(t *testing.T) {
	t.Parallel()
	assert.Equal(t, clierr.Rancher+" delete project 'p-1' --cluster 'c-abc' --dry-run", deleteProjectHint("p-1", "c-abc"))
	assert.Equal(t, clierr.Rancher+" delete project --cluster 'c-abc' --dry-run -- '-p'", deleteProjectHint("-p", "c-abc"))
}
