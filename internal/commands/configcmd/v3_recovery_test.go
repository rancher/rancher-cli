package configcmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

func v3DeleteCommand(name string) string {
	return "rancher delete management-token " + name + " --apply --yes --allow-deprecated"
}

func v3Ref(name string) clierr.TokenRef { return clierr.TokenRef{Kind: clierr.TokenKindV3, Name: name} }

// v3RecoveryConfig holds a v3 token in every state the surfaces show one:
// the current token of an entry with a login, one of a logged-out entry,
// a record naming its own server, and a record of the entry's server.
func v3RecoveryConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(config.EnvCredentialStore, "")
	cfg := &config.Config{TokenReuse: true, Servers: map[string]config.Server{
		"exec-login-r.ex": {URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin, TokenUserID: "user-a",
			TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "kubeconfig-u-cur"},
			SupersededTokens: []config.SupersededToken{
				{Ref: v3Ref("kubeconfig-u-x"), UserID: "user-a", SupersededAt: "2026-09-03T00:00:00Z", Server: "https://old.ex",
					LastAttempt: &clierr.RevokeOutcome{Result: clierr.ResultFailed, Error: "logout 401; delete 403 Forbidden"}},
				{Ref: v3Ref("kubeconfig-u-y"), UserID: "user-a", SupersededAt: "2026-09-04T00:00:00Z"},
			}},
		"out": {URL: "https://o.ex", Provenance: clierr.ProvenanceDerived, TokenUserID: "user-b", TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "kubeconfig-u-out"},
			LoggedOutAt: "2026-09-05T00:00:00Z"},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir
}

// TestConfigStatusGivesAV3TokenTheAdministratorsDelete asserts that every
// v3 token config status shows - a superseded record, the current token of
// an entry with a login, and the current token of a logged-out entry - has
// the administrator's delete as its only recovery command in the data, and
// in the text one line naming the server (the record's own, else the
// entry's), with no login sequence, no assumption line and no auth logout.
func TestConfigStatusGivesAV3TokenTheAdministratorsDelete(t *testing.T) {
	dir := v3RecoveryConfig(t)
	res := execConfigInfoResult(t, dir)
	servers := res.Data.(map[string]any)["servers"].(map[string]any)
	prod := servers["exec-login-r.ex"].(map[string]any)
	recs := prod["supersededTokens"].([]map[string]any)
	require.Len(t, recs, 2)
	assert.Equal(t, []string{clierr.MarkRemedy(v3DeleteCommand("kubeconfig-u-x"))}, recs[0]["recovery"])
	assert.Equal(t, []string{clierr.MarkRemedy(v3DeleteCommand("kubeconfig-u-y"))}, recs[1]["recovery"])
	assert.Equal(t, []string{clierr.MarkRemedy(v3DeleteCommand("kubeconfig-u-cur"))}, prod["currentToken"].(map[string]any)["recovery"])
	out := servers["out"].(map[string]any)
	assert.Equal(t, []string{clierr.MarkRemedy(v3DeleteCommand("kubeconfig-u-out"))}, out["currentToken"].(map[string]any)["recovery"], "a logged-out entry's token")

	assert.Contains(t, res.Text, "        recovery:\n          an administrator logged in to https://old.ex can delete it with: "+v3DeleteCommand("kubeconfig-u-x")+"\n")
	assert.Contains(t, res.Text, "        recovery:\n          an administrator logged in to https://r.ex can delete it with: "+v3DeleteCommand("kubeconfig-u-y")+"\n")
	assert.Contains(t, res.Text, "      recovery:\n        an administrator logged in to https://r.ex can delete it with: "+v3DeleteCommand("kubeconfig-u-cur")+"\n")
	assert.Contains(t, res.Text, "      recovery:\n        an administrator logged in to https://o.ex can delete it with: "+v3DeleteCommand("kubeconfig-u-out")+"\n")
	assert.NotContains(t, res.Text, clierr.RecoverySequenceAssumption)
	assert.NotContains(t, res.Text, "auth logout")
	assert.NotContains(t, res.Text, "auth login")
}

// TestSetTokenReuseOffGivesAV3TokenTheAdministratorsDelete asserts the
// dormant-login report of config set-token-reuse off gives a v3 record and
// a v3 current token the administrator's delete, in the data and in place
// of the revoke-it-with list in the text.
func TestSetTokenReuseOffGivesAV3TokenTheAdministratorsDelete(t *testing.T) {
	dir := v3RecoveryConfig(t)
	res, err := NewSetTokenReuse(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"off"}}, ui.Discard())
	require.NoError(t, err)
	dormant := res.Data.(map[string]any)["dormantLogins"].([]map[string]any)
	require.Len(t, dormant, 1)
	recs := dormant[0]["supersededTokens"].([]map[string]any)
	assert.Equal(t, []string{clierr.MarkRemedy(v3DeleteCommand("kubeconfig-u-x"))}, recs[0]["recovery"])
	assert.Equal(t, []string{clierr.MarkRemedy(v3DeleteCommand("kubeconfig-u-cur"))}, dormant[0]["currentToken"].(map[string]any)["recovery"])
	assert.Contains(t, res.Text, "    an administrator logged in to https://old.ex can delete it with: "+v3DeleteCommand("kubeconfig-u-x")+"\n")
	assert.Contains(t, res.Text, "    an administrator logged in to https://r.ex can delete it with: "+v3DeleteCommand("kubeconfig-u-y"))
	assert.NotContains(t, res.Text, "revoke it with:")
	assert.NotContains(t, res.Text, clierr.RecoverySequenceAssumption)
}
