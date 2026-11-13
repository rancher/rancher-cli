package token

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	sequence := clierr.RecoverySequence("https://hostzq7", ref).Lines()
	item := func(reason string) *revocationItem {
		return &revocationItem{source: sourceLogin, ref: &ref, server: config.CanonicalServerIdentity(stored), recorded: stored, owner: "u-abc12", entry: "x", reason: reason}
	}

	t.Run("an entry that exists", func(t *testing.T) {
		t.Parallel()
		d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{}}
		got := d.advise(item(reasonFailed))
		assert.Contains(t, got.Text, "the server URL in server entry 'x' cannot be used: it holds a user name or password; change the entry's URL with ")
		assert.Empty(t, got.Sequence.Lines())
	})
	t.Run("an entry the command removed", func(t *testing.T) {
		t.Parallel()
		d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{"x": true}}
		got := d.advise(item(reasonFailed))
		assert.Empty(t, got.Text)
		assert.Equal(t, sequence, got.Sequence.Lines())
	})
	t.Run("an entry that is gone from the view", func(t *testing.T) {
		t.Parallel()
		d := &Deletion{cfg: cfg, view: &config.Config{Servers: map[string]config.Server{}}, removed: map[string]bool{}}
		got := d.advise(item(reasonFailed))
		assert.Empty(t, got.Text)
		assert.Equal(t, sequence, got.Sequence.Lines())
	})
	t.Run("a forbidden token on an entry that exists", func(t *testing.T) {
		t.Parallel()
		d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{}}
		got := d.advise(item(reasonForbidden))
		assert.Equal(t, "the server recorded for token 'token-abc12' cannot be used: it holds a user name or password; "+
			"the token stays on the server that issued it; delete it there as user 'u-abc12' while signed in to that server: "+clierr.Rancher+" delete token 'token-abc12' --apply --yes", got.Text)
		assert.Empty(t, got.Sequence.Lines())
	})
}

// TestRecoveryLeavesOutTheSequenceForAServerThatCannotBePrinted asserts the
// recovery of a token on a server whose URL holds a quote, a right single
// quotation mark, a zero width space or two backslashes is the command that
// lists the tokens, with no login user, and that the warning for it names the
// server, gives the list command, carries the note once and holds no delete
// command.
func TestRecoveryLeavesOutTheSequenceForAServerThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for name, host := range map[string]string{
		"quote":                  "o'b.example",
		"right single quotation": "o\u2019b.example",
		"zero width space":       "o\u200bb.example",
		"two backslashes":        `o\\b.example`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := "https://" + host
			ref := TokenRef{Kind: TokenKindExt, Name: "token-abc12"}
			d := &Deletion{cfg: &config.Config{}, view: &config.Config{}, removed: map[string]bool{}}
			it := &revocationItem{source: sourceLogin, ref: &ref, server: config.CanonicalServerIdentity(server), recorded: server, owner: "u-abc12", reason: reasonOtherServer}
			commands, loginAs := d.recovery(it)
			if advice := d.advise(it); advice.Text != "" {
				t.Skipf("the server is refused before a sequence is built: %s", advice.Text)
			}
			require.Len(t, commands, 1)
			assert.Equal(t, "rancher get tokens", clierr.ExpandRemedies(commands[0], ""))
			assert.Empty(t, loginAs)

			warning := clierr.ExpandRemedies(d.manualWarning(it), "")
			assert.Contains(t, warning, "the token stays on "+config.DisplayServerURL(config.CanonicalServerIdentity(server))+"; log in there and run: rancher get tokens; "+clierr.LeftOutNote)
			assert.NotContains(t, warning, "delete token")
			assert.Equal(t, 1, strings.Count(warning, clierr.LeftOutNote))
		})
	}
}
