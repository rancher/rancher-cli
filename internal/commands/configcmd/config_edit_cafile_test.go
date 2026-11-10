package configcmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// TestConfigEditSaysWhatTheFileIsOnceWhateverTheNumberOfChangedServers
// asserts the refusal of an edit that changes the URL of one server, and of
// two servers, names --cacert <file> and says what <file> is once, after the
// clause that names it and before the command of the first changed server,
// never once per changed server.
func TestConfigEditSaysWhatTheFileIsOnceWhateverTheNumberOfChangedServers(t *testing.T) {
	const base = "CurrentServer: prod\nservers:\n  prod:\n    url: https://prod.example.com\n  stage:\n    url: https://stage.example.com\n"
	for _, tc := range []struct {
		name, edited string
		changed      []string
	}{
		{"one changed server", "CurrentServer: prod\nservers:\n  prod:\n    url: https://other.example.com\n  stage:\n    url: https://stage.example.com\n", []string{"servers.prod changed"}},
		{"two changed servers", "CurrentServer: prod\nservers:\n  prod:\n    url: https://other.example.com\n  stage:\n    url: https://another.example.com\n", []string{"servers.prod changed", "servers.stage changed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("EDITOR", "fake-editor")
			dir := t.TempDir()
			seedConfigFile(t, dir, base)
			cmd := &configEditCmd{dir: dir, launchEditor: writesContent(tc.edited)}
			_, err := cmd.Execute(t.Context(), cmdtest.Input{}, &fakePrompter{canPrompt: true, answers: []string{"n"}})
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			msg := ce.Message
			for _, changed := range tc.changed {
				assert.Contains(t, msg, changed)
			}
			remedytest.AssertClauseOnce(t, msg, config.CAFileMeaning, config.CAFileArg("--cacert"), false)
			assert.Less(t, strings.Index(msg, config.CAFileMeaning), strings.Index(msg, "; use: "), "and comes before the command of the first changed server: %s", msg)
		})
	}
}
