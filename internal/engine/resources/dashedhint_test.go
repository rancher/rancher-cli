package resources

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestDeleteProjectHintPreviewsTheDelete asserts the preview of deleting a
// project, for an ordinary id and for an id that starts with a dash.
func TestDeleteProjectHintPreviewsTheDelete(t *testing.T) {
	t.Parallel()
	assert.Equal(t, clierr.Rancher+" delete project 'p-1' --cluster 'c-abc' --dry-run", deleteProjectHint("p-1", "c-abc").String())
	assert.Equal(t, clierr.Rancher+" delete project --cluster 'c-abc' --dry-run -- '-p'", deleteProjectHint("-p", "c-abc").String())
}

// TestDeleteProjectReadFailureLeavesOutThePreviewOfAnUnprintableValue asserts
// the refusal of a failed namespace read hints the preview of deleting the
// project and says so, and with a project id or cluster no shell reads as
// written leaves the command and the sentence about it out and carries the
// note.
func TestDeleteProjectReadFailureLeavesOutThePreviewOfAnUnprintableValue(t *testing.T) {
	t.Parallel()
	read := func(t *testing.T, project, cluster string) *clierr.Error {
		t.Helper()
		ce, ok := errors.AsType[*clierr.Error](readFailure(errors.New("boom"), project, cluster))
		require.True(t, ok)
		return ce
	}
	ce := read(t, "p-1", "c-abc")
	assert.Equal(t, clierr.Rancher+" delete project 'p-1' --cluster 'c-abc' --dry-run", ce.Hint)
	assert.Contains(t, ce.Message, deleteProjectHintNote(deleteProjectHint("p-1", "c-abc")))
	assert.NotContains(t, ce.Message, clierr.LeftOutNote)
	for name, tc := range map[string]struct{ project, cluster string }{
		"project apostrophe":       {"p'1", "c-abc"},
		"project right quote":      {"p\u20191", "c-abc"},
		"cluster zero width":       {"p-1", "c\u200b-abc"},
		"cluster double backslash": {"p-1", "c\\\\abc"},
	} {
		t.Run(name, func(t *testing.T) {
			ce := read(t, tc.project, tc.cluster)
			assert.Empty(t, ce.Hint)
			assert.NotContains(t, ce.Message, "the hint previews")
			assert.Contains(t, ce.Message, clierr.LeftOutNote)
		})
	}
}
