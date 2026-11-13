package token

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestLeftOutRecoverySequenceKeepsNeitherTheAssumptionNorTheCATail asserts that
// a warning about a record on a server whose URL holds a single quote, whose
// login in the recovery sequence is left out, says nothing about what the
// sequence assumes and carries no clause about a private CA, gives the command
// that lists the tokens and holds the note once. Mutation: write the
// assumption sentence or the CA clause when the sequence is left out.
func TestLeftOutRecoverySequenceKeepsNeitherTheAssumptionNorTheCATail(t *testing.T) {
	t.Parallel()
	const server = "https://o'b.example"
	ref := TokenRef{Kind: TokenKindExt, Name: "token-abc12"}
	cfg := &config.Config{Servers: map[string]config.Server{}}
	assertLeftOut := func(t *testing.T, text string) {
		t.Helper()
		assertDeleteLeftOut(t, text, TokenKindExt)
		expanded := clierr.ExpandRemedies(text, "")
		assert.NotContains(t, expanded, "the sequence")
		assert.NotContains(t, expanded, clierr.RecoverySequenceAssumption)
		assert.NotContains(t, expanded, "--cacert")
		assert.NotContains(t, expanded, config.EnvCACert)
	}

	t.Run("a superseded token that was kept", func(t *testing.T) {
		t.Parallel()
		assertLeftOut(t, keptWarning(config.SupersededToken{Ref: ref, UserID: "u-abc12"}, server, "the server refused"))
	})
	for name, it := range map[string]*revocationItem{
		"a record no entry names": {source: sourceCache, ref: &ref, server: server, owner: "u-abc12", reason: reasonNoEntry},
		"an unknown authority": {source: sourceCache, ref: &ref, server: server, owner: "u-abc12", reason: reasonFailed,
			cert: &tlstrust.Details{Kind: tlstrust.KindUnknownAuthority}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{}}
			assertLeftOut(t, d.manualWarning(it))
		})
	}
}

// TestLeftOutLoginNeverPrintsTheRevokeThatFollowsIt asserts that a recovery
// for a token that needs a new login first, for a server entry whose name holds
// a single quote, prints no delete command of either kind, gives the command
// that lists the tokens of the kind, and holds the note once. Mutation: write
// the revoke after a login that was left out.
func TestLeftOutLoginNeverPrintsTheRevokeThatFollowsIt(t *testing.T) {
	t.Parallel()
	const entry = "it's"
	for _, kind := range []clierr.TokenKind{TokenKindExt, TokenKindV3} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			ref := TokenRef{Kind: kind, Name: "token-abc12"}
			text, hint := ResidualRecovery(clierr.TokenFate{Ref: &ref}, entry, "m", false)
			assertDeleteLeftOut(t, text, kind)
			assert.Contains(t, text, "no credential was stored")
			assert.Contains(t, text, "token-abc12")
			assert.Empty(t, hint.Field())
		})
	}
}

// TestLeftOutLoginNoteOfAMarkedTokenAppearsOnce asserts that the recovery for
// a token known only by its marker, after a login that was left out, holds the
// note once however many of its commands are left out. Mutation: join the
// login text and the steps without clierr.NoteOnce.
func TestLeftOutLoginNoteOfAMarkedTokenAppearsOnce(t *testing.T) {
	t.Parallel()
	for _, kind := range []clierr.TokenKind{TokenKindExt, TokenKindV3, ""} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			text, _ := ResidualRecovery(clierr.TokenFate{Marked: true, Kind: kind}, "it's", "m", false)
			assert.Equal(t, 1, strings.Count(clierr.ExpandRemedies(text, ""), clierr.LeftOutNote), text)
		})
	}
}
