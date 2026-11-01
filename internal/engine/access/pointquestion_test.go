package access

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestPointQuestionTextsAreCompleteCommands asserts the point question a
// refusal suggests: it names a verb, a resource and a cluster, and a group is
// named with --type group, because a bare name is read as a user.
func TestPointQuestionTextsAreCompleteCommands(t *testing.T) {
	t.Parallel()
	assert.Equal(t, clierr.Rancher+" access can <verb> <resource> --cluster <cluster> --as 'eng' --type group", groupAccessCan("eng"))
	assert.Equal(t, clierr.Rancher+" access can <verb> <resource> --cluster <cluster> --as '-eng' --type group", groupAccessCan("-eng"))
	assert.Equal(t, clierr.Rancher+" access can <verb> <resource> --cluster <cluster> --as system:anonymous", anonymousAccessCan())
	assert.Equal(t, "the management cluster cannot impersonate; <verb> and <resource> are what to ask; ask with: "+
		clierr.Rancher+" access can <verb> <resource> --cluster local", localCannotImpersonateNote)
}
