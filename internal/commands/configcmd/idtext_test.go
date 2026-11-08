package configcmd

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/remedytest"
)

// TestRecordOwnerWithHiddenRunesIsShownAsEscapes asserts the text of config
// status for a record whose recorded owner holds a direction override, a
// zero-width space, a bell, a newline and the start of a remedy marker shows
// the owner with escapes, never with the runes removed, in the recovery field
// and in the record lines.
func TestRecordOwnerWithHiddenRunesIsShownAsEscapes(t *testing.T) {
	t.Parallel()
	owner := "u-" + string(rune(0x202e)) + string(rune(0x200b)) + "\a\n" + string(rune(0x2063)) + "rancher-x"
	quoted := clierr.ShellQuote(strconv.Quote(owner))
	ext := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc12"}
	const server = "https://hostzq7"

	lines, ok := recoveryField("x", server, server, owner, ext).val.([]string)
	require.True(t, ok)
	field := strings.Join(lines, "\n")
	assert.Contains(t, field, "log in as user "+quoted+" (the token's owner), then run:")

	s := config.Server{URL: server, SupersededTokens: []config.SupersededToken{{Ref: ext, UserID: owner, SupersededAt: "2026-09-03T00:00:00Z"}}}
	record := strings.Join(recordLines("x", s, ""), "\n")
	assert.Contains(t, record, "log in as user "+quoted+" (the token's owner), then revoke it with:")

	for _, text := range []string{field, record} {
		remedytest.AssertIDShownWithEscapes(t, text, owner)
	}
}

// TestAConfigStatusBlockWritesAnOwnerTheSameInEveryLine asserts that one
// config status block writes a record's owner the same way in its header, its
// recovery line and its userId field, and the entry's owner the same way in
// the tokenUserId field and the current token's lines: a normal id as it is in
// every line, an id with hidden runes as one double-quoted string with
// escapes in every line, so no line shows another id than the others.
func TestAConfigStatusBlockWritesAnOwnerTheSameInEveryLine(t *testing.T) {
	t.Parallel()
	ext := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc12"}
	const server = "https://hostzq7"
	for _, tc := range []struct{ name, owner, shown string }{
		{"a normal id", "u-abc12", "u-abc12"},
		{"hidden runes", "u-abc" + string(rune(0x200b)) + "12" + string(rune(0x202e)), strconv.Quote("u-abc" + string(rune(0x200b)) + "12" + string(rune(0x202e)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := config.Server{URL: server, Provenance: clierr.ProvenanceDerived, TokenUserID: tc.owner, TokenRef: &ext, CredentialExpiresAt: "2026-12-01T00:00:00Z",
				SupersededTokens: []config.SupersededToken{{Ref: ext, UserID: tc.owner, SupersededAt: "2026-09-03T00:00:00Z"}}}

			lines := recordLines("x", s, "")
			require.GreaterOrEqual(t, len(lines), 3)
			header, recovery := lines[0], ""
			for _, l := range lines {
				if strings.Contains(l, "log in as user ") {
					recovery = l
				}
			}
			assert.Contains(t, header, "("+tc.shown+", superseded ")
			assert.Contains(t, recovery, "log in as user "+clierr.ShellQuote(tc.shown)+" (the token's owner)")

			var text strings.Builder
			renderFields(entrySection("x", s), "", &text)
			userIDLines := 0
			for l := range strings.SplitSeq(text.String(), "\n") {
				if strings.TrimSpace(l) == "userId: "+tc.shown {
					userIDLines++
				}
			}
			assert.Equal(t, 2, userIDLines, "the current token and the record write the owner alike")
			for _, want := range []string{
				"tokenUserId: " + tc.shown + "\n",
				"log in as user " + clierr.ShellQuote(tc.shown) + " (the token's owner), then run:",
			} {
				assert.Contains(t, text.String(), want)
			}
			if tc.owner != tc.shown {
				remedytest.AssertIDShownWithEscapes(t, text.String(), tc.owner)
				remedytest.AssertIDShownWithEscapes(t, strings.Join(lines, "\n"), tc.owner)
			}
		})
	}
}

// TestRecordLinesEscapeAnOwnerWhenTheRecordServerCannotBeUsed asserts the
// config status lines of a record whose server does not parse show an owner
// with hidden runes as escapes, never with the runes removed.
func TestRecordLinesEscapeAnOwnerWhenTheRecordServerCannotBeUsed(t *testing.T) {
	t.Parallel()
	const owner = "u-abc\u200b12"
	ext := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc12"}
	s := config.Server{URL: "https://hostzq7", SupersededTokens: []config.SupersededToken{{Ref: ext, UserID: owner, Server: "not a url", SupersededAt: "2026-09-03T00:00:00Z"}}}
	lines := recordLines("x", s, "")
	text := strings.Join(lines, "\n")
	assert.Contains(t, text, "delete it there as user "+clierr.ShellQuote(strconv.Quote(owner))+" while signed in")
	remedytest.AssertIDShownWithEscapes(t, text, owner)
}
