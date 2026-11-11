package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// TestCommittedContextRecoveryForAnUnprintableDirectory asserts the recovery
// text of a post-commit failure, rendered for a directory no command can
// name: a text whose command sits in its second clause keeps the first
// clause and ends with the note, and a text whose command sits in its first
// clause is exactly the note, never an empty recover line.
func TestCommittedContextRecoveryForAnUnprintableDirectory(t *testing.T) {
	t.Parallel()
	streams := IO{RemedyDir: "/home/u/a\nb"}
	for name, tc := range map[string]struct{ recovery, want string }{
		"command in the second clause": {
			"check the token is gone; then run " + clierr.Rancher + " get tokens",
			"check the token is gone; " + UnprintableConfigDirNote,
		},
		"command in the first clause": {
			"run " + clierr.Rancher + " get tokens and delete the new one",
			UnprintableConfigDirNote,
		},
		"command only": {
			clierr.Rancher + " get tokens",
			UnprintableConfigDirNote,
		},
		"no command": {
			"rotate the same name again once the controller is healthy",
			"rotate the same name again once the controller is healthy",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ce := clierr.New(clierr.CodeGeneric, "rendering failed after the mutation committed").
				WithDetails(clierr.CommittedContextDetails{Committed: true, ResourceKind: "Token", ResourceName: "t-1", Recovery: tc.recovery})
			got := streams.ExpandError(ce)
			details, ok := got.Details.(clierr.CommittedContextDetails)
			require.True(t, ok)
			assert.Equal(t, tc.want, details.Recovery)
			lines := remedytest.DetailTexts(details.TextDetails())
			assert.Equal(t, "recover: "+tc.want, lines[len(lines)-1])
			assert.NotContains(t, details.Recovery, clierr.RemedyMarker)
		})
	}
}
