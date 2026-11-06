package configcmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestConfigTokenRecoveryTextsNameTheTokenTheCommandAndWhoRunsIt asserts, for
// each text config status and config set-token-reuse print for a token that
// stays on a server, the token, the command, and the person who can run it:
// the token's owner when the record names it, the token's owner or an
// administrator when it does not, an administrator for a v3 token. The key of
// a row is the function that builds the text, as the guard in package token
// names it; each person is written here as the words of the text.
func TestConfigTokenRecoveryTextsNameTheTokenTheCommandAndWhoRunsIt(t *testing.T) {
	t.Parallel()
	const (
		server    = "https://hostzq7"
		deleteExt = "rancher delete token 'token-abc12' --apply --yes"
		deleteV3  = "rancher delete management-token 'kubeconfig-u-x' --apply --yes --allow-deprecated"
	)
	ext := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc12"}
	v3 := clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "kubeconfig-u-x"}
	record := func(url, recordServer, userID string, ref clierr.TokenRef) string {
		s := config.Server{URL: url, SupersededTokens: []config.SupersededToken{{Ref: ref, UserID: userID, SupersededAt: "2026-09-03T00:00:00Z", Server: recordServer}}}
		return strings.Join(recordLines("x", s, ""), "\n")
	}
	fieldText := func(entryURL, serverURL, userID string, ref clierr.TokenRef) string {
		lines, ok := recoveryField("x", entryURL, serverURL, userID, ref).val.([]string)
		require.True(t, ok)
		return strings.Join(lines, "\n")
	}
	for _, tc := range []struct {
		key     string
		name    string
		text    string
		noun    string
		command string
		person  string
	}{
		{"configcmd.administratorLine", "a v3 token", administratorLine(server, v3), "kubeconfig-u-x", deleteV3, "an administrator logged in to"},

		{"configcmd.sequenceLines", "a known owner", strings.Join(sequenceLines("", "u-abc12", clierr.RecoverySequence(server, ext)), "\n"),
			"token-abc12", deleteExt, "log in as user 'u-abc12' (the token's owner), then revoke it with:"},
		{"configcmd.sequenceLines", "no known owner", strings.Join(sequenceLines("", "", clierr.RecoverySequence(server, ext)), "\n"),
			"token-abc12", deleteExt, "log in as the token's owner or an administrator, then revoke it with:"},

		{"configcmd.recordLines", "an ext token, a known owner", record(server, "", "u-abc12", ext),
			"token-abc12", deleteExt, "log in as user 'u-abc12' (the token's owner), then revoke it with:"},
		{"configcmd.recordLines", "an ext token, no known owner", record(server, "", "", ext),
			"token-abc12", deleteExt, "log in as the token's owner or an administrator, then revoke it with:"},
		{"configcmd.recordLines", "a v3 token", record(server, "", "u-abc12", v3), "kubeconfig-u-x", deleteV3, "an administrator logged in to"},
		{"configcmd.recordLines", "no server recorded, a known owner", record("", "", "u-abc12", ext), "token-abc12", deleteExt, "user 'u-abc12'"},
		{"configcmd.recordLines", "no server recorded, no known owner", record("", "", "", ext), "token-abc12", deleteExt, "its owner or an administrator"},
		{"configcmd.recordLines", "a server that cannot be used, a known owner", record(server, "unusable:https://old.ex:abc", "u-abc12", ext), "token-abc12", deleteExt, "user 'u-abc12'"},
		{"configcmd.recordLines", "a server that cannot be used, no known owner", record(server, "unusable:https://old.ex:abc", "", ext), "token-abc12", deleteExt, "its owner or an administrator"},

		{"configcmd.recoveryField", "an ext token, a known owner", fieldText(server, server, "u-abc12", ext),
			"token-abc12", deleteExt, "log in as user 'u-abc12' (the token's owner), then run:"},
		{"configcmd.recoveryField", "an ext token, no known owner", fieldText(server, server, "", ext),
			"token-abc12", deleteExt, "log in as the token's owner or an administrator, then run:"},
		{"configcmd.recoveryField", "a v3 token", fieldText(server, server, "u-abc12", v3), "kubeconfig-u-x", deleteV3, "an administrator logged in to"},
		{"configcmd.recoveryField", "no server recorded, a known owner", fieldText(server, "", "u-abc12", ext), "token-abc12", deleteExt, "user 'u-abc12'"},
		{"configcmd.recoveryField", "no server recorded, no known owner", fieldText(server, "", "", ext), "token-abc12", deleteExt, "its owner or an administrator"},
		{"configcmd.recoveryField", "a server that cannot be used, a known owner", fieldText(server, "unusable:https://old.ex:abc", "u-abc12", ext), "token-abc12", deleteExt, "user 'u-abc12'"},
		{"configcmd.recoveryField", "a server that cannot be used, no known owner", fieldText(server, "unusable:https://old.ex:abc", "", ext), "token-abc12", deleteExt, "its owner or an administrator"},
	} {
		t.Run(tc.key+", "+tc.name, func(t *testing.T) {
			t.Parallel()
			text := clierr.ExpandRemedies(tc.text, "")
			assert.Contains(t, text, tc.noun)
			assert.Contains(t, text, tc.command)
			assert.Contains(t, text, tc.person)
			assert.NotContains(t, text, "only an administrator")
		})
	}
}
