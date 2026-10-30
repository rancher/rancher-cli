package configcmd

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConfigStatusListsWhatALogoutLeaves asserts the entry states a logout
// leaves: an entry whose TokenRef was cleared and LoggedOutAt stamped is
// listed only for its superseded records and shows no current token; a SAML
// legacy entry (CLI-created, never named) is listed with the "cannot revoke
// by name" note while it holds a login and not at all once logged out; an
// entry logged out with its TokenRef kept is listed with that token.
func TestConfigStatusListsWhatALogoutLeaves(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvCredentialStore, "")
	rec := config.SupersededToken{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-left"}, UserID: "user-a",
		SupersededAt: "2026-10-06T00:00:00Z", Server: "https://r.ex"}
	cfg := &config.Config{Servers: map[string]config.Server{
		"logged-out": {URL: "https://r.ex", Provenance: clierr.ProvenanceDerived, TokenUserID: "user-a",
			LoggedOutAt: "2026-10-06T00:00:00Z", SupersededTokens: []config.SupersededToken{rec}},
		"logged-out-empty": {URL: "https://r.ex", Provenance: clierr.ProvenanceDerived, TokenUserID: "user-a", LoggedOutAt: "2026-10-06T00:00:00Z"},
		"saml-in":          {URL: "https://s.ex", Provenance: clierr.ProvenanceLegacySession},
		"saml-out":         {URL: "https://s.ex", Provenance: clierr.ProvenanceLegacySession, LoggedOutAt: "2026-10-06T00:00:00Z"},
		"earlier-logout": {URL: "https://r.ex", Provenance: clierr.ProvenanceDerived, TokenUserID: "user-a",
			TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-kept"}, LoggedOutAt: "2026-09-01T00:00:00Z"},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	res := execConfigInfoResult(t, dir)
	servers := res.Data.(map[string]any)["servers"].(map[string]any)
	assert.ElementsMatch(t, []string{"logged-out", "saml-in", "earlier-logout"}, slices.Collect(maps.Keys(servers)))

	out := servers["logged-out"].(map[string]any)
	assert.NotContains(t, out, "currentToken")
	recs := out["supersededTokens"].([]map[string]any)
	require.Len(t, recs, 1)
	assert.Equal(t, "token-left", recs[0]["ref"].(map[string]any)["name"])
	assert.Equal(t, "sameOwner", recs[0]["state"])

	saml := servers["saml-in"].(map[string]any)
	assert.Equal(t, "cannot revoke by name: no tokenRef recorded", saml["currentToken"].(map[string]any)["note"])

	earlier := servers["earlier-logout"].(map[string]any)
	assert.Equal(t, "token-kept", earlier["currentToken"].(map[string]any)["ref"].(map[string]any)["name"])
	assert.Equal(t, 1, strings.Count(res.Text, "cannot revoke by name"), "only the SAML entry that holds a login carries the note")
}

// TestConfigStatusShowsARecordOfAnotherServer asserts a record whose server
// is not the entry's current one: its state says so and its recovery
// sequence logs in to the record's server, not the entry's; a record
// without a server belongs to the entry's.
func TestConfigStatusShowsARecordOfAnotherServer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvCredentialStore, "")
	cfg := &config.Config{Servers: map[string]config.Server{
		"prod": {URL: "https://new.ex", Provenance: clierr.ProvenanceDerived, TokenUserID: "user-a",
			TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-now"},
			SupersededTokens: []config.SupersededToken{
				{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old-server"}, UserID: "user-a", SupersededAt: "2026-10-06T00:00:00Z", Server: "https://old.ex"},
				{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-no-server"}, UserID: "user-a", SupersededAt: "2026-10-06T00:00:00Z"},
			}},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	res := execConfigInfoResult(t, dir)
	recs := res.Data.(map[string]any)["servers"].(map[string]any)["prod"].(map[string]any)["supersededTokens"].([]map[string]any)
	require.Len(t, recs, 2)
	assert.Equal(t, "otherServer", recs[0]["state"])
	assert.Equal(t, "https://old.ex", recs[0]["server"])
	assert.Equal(t, clierr.MarkRemedy("rancher auth login 'recover-old.ex' --url 'https://old.ex'   # log in as user 'user-a'"), recs[0]["recovery"].([]string)[0])
	assert.Equal(t, "sameOwner", recs[1]["state"])
	assert.NotContains(t, recs[1], "server")
	assert.Equal(t, clierr.MarkRemedy("rancher auth login 'recover-new.ex' --url 'https://new.ex'   # log in as user 'user-a'"), recs[1]["recovery"].([]string)[0])
	assert.Contains(t, res.Text, "      ext/token-old-server:\n        userId: user-a\n        server: https://old.ex\n")
	assert.Contains(t, res.Text, "        state: otherServer\n")
	assert.Contains(t, res.Text, "rancher auth login 'recover-old.ex' --url 'https://old.ex'")
}

// TestSetTokenReuseOffOffersNoLogoutOfALoggedOutPluginEntry asserts the
// dormant-login list after a logout: a plugin login entry whose TokenRef
// the logout cleared is told how to remove the entry, never to log out a
// credential it no longer holds.
func TestSetTokenReuseOffOffersNoLogoutOfALoggedOutPluginEntry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvCredentialStore, "")
	cfg := &config.Config{TokenReuse: true, Servers: map[string]config.Server{
		"exec-login-r.ex": {URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin, TokenUserID: "user-a", LoggedOutAt: "2026-10-06T00:00:00Z"},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	res, err := NewSetTokenReuse(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"off"}}, ui.Discard())
	require.NoError(t, err)
	assert.NotContains(t, res.Text, "auth logout")
	assert.Contains(t, res.Text, "config delete-server 'exec-login-r.ex'")
}

// TestSetTokenReuseOffGivesARecordOfAnotherServerItsServer asserts that the
// dormant-login text's recovery for a superseded record logs in to the
// server the record names, not the entry's current one.
func TestSetTokenReuseOffGivesARecordOfAnotherServerItsServer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvCredentialStore, "")
	cfg := &config.Config{TokenReuse: true, Servers: map[string]config.Server{
		"exec-login-r.ex": {URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin, TokenUserID: "user-a", LoggedOutAt: "2026-10-06T00:00:00Z",
			SupersededTokens: []config.SupersededToken{
				{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old-server"}, UserID: "user-a", SupersededAt: "2026-10-06T00:00:00Z", Server: "https://old.ex"},
			}},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	res, err := NewSetTokenReuse(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"off"}}, ui.Discard())
	require.NoError(t, err)
	assert.Contains(t, res.Text, "rancher auth login 'recover-old.ex' --url 'https://old.ex'")
	assert.NotContains(t, res.Text, "--url 'https://r.ex'")
}
