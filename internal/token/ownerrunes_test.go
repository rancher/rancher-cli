package token

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
)

// TestRevocationOwnerIsEscapedInTheWarningAndExactInTheReport asserts an owner
// id that holds a direction override, a line separator and a paragraph
// separator stays exact in the loginAs field of the JSON report, and the
// warning text that names the user the sequence logs in as shows it with
// escapes.
func TestRevocationOwnerIsEscapedInTheWarningAndExactInTheReport(t *testing.T) {
	t.Parallel()
	const url = "https://r.example.com"
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: url}}}
	ref := &TokenRef{Kind: TokenKindExt, Name: "token-abc"}
	it := &revocationItem{source: sourceCache, ref: ref, server: config.CanonicalServerIdentity(url),
		owner: "u-a\u202eb\u2028c\u2029d", entry: "prod", reason: reasonFailed}
	d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{"prod": true}, items: []*revocationItem{it}}

	rep := d.Report()
	env := output.NewEnvelope("LogoutResult", map[string]any{"revoked": rep.Revoked, "notRevoked": rep.NotRevoked})
	got := renderAndRead(t, "json", env)
	items, ok := got.Data["notRevoked"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	failed, ok := items[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "u-a\u202eb\u2028c\u2029d", failed["loginAs"])

	warning := clierr.ExpandRemedies(d.manualWarning(it), "")
	assert.Contains(t, warning, `revoke it as user '"u-a\u202eb\u2028c\u2029d"' with: `)
	assert.Equal(t, warning, output.Printable(warning), "the warning holds no rune the printer removes")
}

// TestRevocationOwnerOfOnlyUnsafeRunesIsShownAsEscapes asserts an owner id made
// only of format runes is a known owner: the loginAs field of the JSON report
// holds it exactly and the warning shows it with escapes, never an empty user
// name and never another user.
func TestRevocationOwnerOfOnlyUnsafeRunesIsShownAsEscapes(t *testing.T) {
	t.Parallel()
	const url = "https://r.example.com"
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: url}}}
	ref := &TokenRef{Kind: TokenKindExt, Name: "token-abc"}
	it := &revocationItem{source: sourceCache, ref: ref, server: config.CanonicalServerIdentity(url),
		owner: "\u202e\u2063", entry: "prod", reason: reasonFailed}
	d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{"prod": true}, items: []*revocationItem{it}}

	rep := d.Report()
	env := output.NewEnvelope("LogoutResult", map[string]any{"revoked": rep.Revoked, "notRevoked": rep.NotRevoked})
	got := renderAndRead(t, "json", env)
	items, ok := got.Data["notRevoked"].([]any)
	require.True(t, ok)
	require.Len(t, items, 1)
	failed, ok := items[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "\u202e\u2063", failed["loginAs"])
	assert.NotEmpty(t, failed["recovery"])

	warning := clierr.ExpandRemedies(d.manualWarning(it), "")
	assert.Contains(t, warning, `revoke it as user '"\u202e\u2063"' with: `)
}

// TestNextLoginWarningEscapesTheEntryOwner asserts the warning that names the
// login revoking an entry's tokens, in the login form and the kubectl
// sign-in form, carries the entry's owner id with every direction-changing,
// invisible or separator rune as an escape in the JSON envelope, never
// removed.
func TestNextLoginWarningEscapesTheEntryOwner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, owner, login, signIn string
	}{
		{"escaped", "u-a\u202eb\u2028c",
			`the next login of prod as "u-a\u202eb\u2028c"; run it with: `,
			`the next kubectl sign-in through plugin as "u-a\u202eb\u2028c"`},
		{"only hidden runes", "\u202e\u2063",
			`the next login of prod as "\u202e\u2063"; run it with: `,
			`the next kubectl sign-in through plugin as "\u202e\u2063"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := textSafetyDeletion(func(p string) string {
				if p == "TokenUserID" {
					return tc.owner
				}
				return "<" + p + ">"
			})
			env := output.NewEnvelope("LogoutResult", map[string]any{})
			env.Warnings = d.Warnings()
			got := renderAndRead(t, "json", env)
			var login, signIn string
			for _, w := range got.Warnings {
				switch {
				case strings.Contains(w, "recorded on prod"):
					login = w
				case strings.Contains(w, "recorded on plugin"):
					signIn = w
				}
			}
			assert.Contains(t, login, tc.login)
			assert.Contains(t, signIn, tc.signIn)
			assert.NotContains(t, login+signIn, "\u202e")
			assert.NotContains(t, login+signIn, "\u2028")
		})
	}
}
