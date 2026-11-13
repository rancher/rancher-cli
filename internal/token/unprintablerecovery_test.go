package token

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestRecoveryTextsCarryTheNoteForACommandThatWasLeftOut asserts that when the
// entry name of a failed login cannot be printed in a command, the recovery
// command of the local credential is empty, and the recovery text of the
// credential and of the config entry says the command is left out, once.
func TestRecoveryTextsCarryTheNoteForACommandThatWasLeftOut(t *testing.T) {
	t.Parallel()
	for name, entry := range map[string]string{
		"zero width space":       "abc\u200b",
		"quote":                  "a'b",
		"right single quotation": "a\u2019b",
		"two backslashes":        `a\\b`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cr := credentialRecoveryFor(LoginStates{Commit: &config.CommitOutcome{Backend: clierr.BackendFile, State: config.CommitPossiblyCommitted}}, entry)
			require.NotNil(t, cr)
			assert.Empty(t, cr.Command)
			text := credentialRecoveryText(cr, entry)
			assert.Equal(t, 1, strings.Count(text, clierr.LeftOutNote))

			configText := configRecoveryText(&clierr.ConfigRecovery{Entry: entry})
			assert.Equal(t, 1, strings.Count(configText, clierr.LeftOutNote))
			assert.NotContains(t, configText, "remove it with")
		})
	}
	printable := configRecoveryText(&clierr.ConfigRecovery{Entry: "prod", Command: "c"})
	assert.Equal(t, "the config entry prod could not be restored to its prior state; remove it with: c", printable)
}
