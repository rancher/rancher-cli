package commands

import (
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetTokenReuseOnOff asserts the two states persist under the config
// lock and the result reports the new value. Any other word never reaches
// Execute: the pipeline rejects an argument outside the ArgSpec enum.
func TestSetTokenReuseOnOff(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cmd := NewSetTokenReuse(dir)
	res, err := cmd.(runtime.ReadCommand).Execute(t.Context(), args{a: []string{"on"}}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, true, res.Data.(map[string]any)["tokenReuse"])
	cfg, _ := config.Load(config.FilePath(dir))
	assert.True(t, cfg.TokenReuse)

	res, err = cmd.(runtime.ReadCommand).Execute(t.Context(), args{a: []string{"off"}}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, false, res.Data.(map[string]any)["tokenReuse"])
	cfg, _ = config.Load(config.FilePath(dir))
	assert.False(t, cfg.TokenReuse)
	assert.Contains(t, res.Text, "token reuse off")
}

// TestSetTokenReuseOffListsDormantPluginLogins asserts the off path names
// every plugin login entry that still holds a credential, its records,
// both removal commands in order, and the recovery sequences; a logged-out
// plugin entry is listed for its records only.
func TestSetTokenReuseOffListsDormantPluginLogins(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.Config{TokenReuse: true, Servers: map[string]config.Server{
		"exec-login-r.ex": {URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin,
			TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-live"}, TokenUserID: "user-a", CredentialExpiresAt: "2026-10-01T00:00:00Z",
			SupersededTokens: []config.SupersededToken{{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old"}, UserID: "user-b", SupersededAt: "2026-09-01T00:00:00Z"}}},
		"prod": {URL: "https://r.ex", Provenance: clierr.ProvenanceDerived, TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-user"}},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	res, err := NewSetTokenReuse(dir).(runtime.ReadCommand).Execute(t.Context(), args{a: []string{"off"}}, ui.Discard())
	require.NoError(t, err)
	dormant := res.Data.(map[string]any)["dormantLogins"].([]map[string]any)
	require.Len(t, dormant, 1, "only plugin login entries; the user entry is not governed by the setting")
	assert.Equal(t, "exec-login-r.ex", dormant[0]["server"])
	assert.Equal(t, "user-a", dormant[0]["tokenUserId"])
	recs := dormant[0]["supersededTokens"].([]map[string]any)
	require.Len(t, recs, 1)
	assert.Equal(t, "differentOwner", recs[0]["state"])
	seq := recs[0]["recovery"].([]string)
	assert.Equal(t, "rancher config use-server 'recover-r.ex'", seq[1])
	text := res.Text
	logout := strings.Index(text, "rancher auth logout 'exec-login-r.ex'")
	del := strings.Index(text, "rancher config delete-server 'exec-login-r.ex'")
	assert.True(t, logout > 0 && del > logout, "both removal commands, logout first: %s", text)
	assert.Contains(t, text, "token-old")
}

// TestSetTokenReuseCompletesOnOff asserts the positional enum is declared
// so the static completion layer owns it.
func TestSetTokenReuseCompletesOnOff(t *testing.T) {
	t.Parallel()
	d := NewSetTokenReuse("").Describe()
	require.Len(t, d.Args, 1)
	assert.Equal(t, []string{"on", "off"}, d.Args[0].Enum)
	assert.True(t, d.Args[0].Required)
	assert.Equal(t, "config set-token-reuse", d.Name)
	assert.Equal(t, runtime.Local, d.CommandType)
}
