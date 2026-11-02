package token

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestAdviseJudgesWhereATokenIsReported asserts what a manual token's advice is
// for an entry whose URL holds user information and canonicalizes: while the
// entry exists it is the entry's repair; for a token whose entry is removed it
// is the recovery sequence of the identity; for a forbidden token on an
// entry that exists (no entry is reported for it) it is the text for the
// record, as the reason of the entry's own URL gives it.
func TestAdviseJudgesWhereATokenIsReported(t *testing.T) {
	t.Parallel()
	const stored = "https://u:pwzq7@hostzq7"
	ref := TokenRef{Kind: TokenKindExt, Name: "token-abc12"}
	cfg := &config.Config{Servers: map[string]config.Server{"x": {URL: stored}}}
	sequence := clierr.RecoverySequence("https://hostzq7", ref)
	item := func(reason string) *revocationItem {
		return &revocationItem{source: sourceLogin, ref: &ref, server: config.CanonicalServerIdentity(stored), recorded: stored, owner: "u-abc12", entry: "x", reason: reason}
	}

	t.Run("an entry that exists", func(t *testing.T) {
		t.Parallel()
		d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{}}
		got := d.advise(item(reasonFailed))
		assert.Contains(t, got.Text, "the server URL in server entry 'x' cannot be used: it holds a user name or password; change the entry's URL with ")
		assert.Empty(t, got.Sequence)
	})
	t.Run("an entry the command removed", func(t *testing.T) {
		t.Parallel()
		d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{"x": true}}
		got := d.advise(item(reasonFailed))
		assert.Empty(t, got.Text)
		assert.Equal(t, sequence, got.Sequence)
	})
	t.Run("an entry that is gone from the view", func(t *testing.T) {
		t.Parallel()
		d := &Deletion{cfg: cfg, view: &config.Config{Servers: map[string]config.Server{}}, removed: map[string]bool{}}
		got := d.advise(item(reasonFailed))
		assert.Empty(t, got.Text)
		assert.Equal(t, sequence, got.Sequence)
	})
	t.Run("a forbidden token on an entry that exists", func(t *testing.T) {
		t.Parallel()
		d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{}}
		got := d.advise(item(reasonForbidden))
		assert.Equal(t, "the server recorded for token 'token-abc12' cannot be used: it holds a user name or password; "+
			"the token stays on the server that issued it; delete it there while signed in to that server: "+clierr.Rancher+" delete token 'token-abc12' --apply --yes", got.Text)
		assert.Empty(t, got.Sequence)
	})
}
