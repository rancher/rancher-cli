package configcmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestTheWarningOfAKeptTokenLeavesOutTheDeleteOfAKeyThatCannotBePrinted asserts
// that the warning for a token the rewrite of a URL keeps in the token cache
// names the command that lists tokens, gives no auth delete-token command for
// a kept key that cannot be printed exactly, and holds the note once; and that
// the warning for a record of a call with a user, which names no delete
// command, holds no note.
func TestTheWarningOfAKeptTokenLeavesOutTheDeleteOfAKeyThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	ref := clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc12"}
	for name, key := range map[string]string{"a single quote": "kept-token:https://r.ex/ext/a'b", "two backslashes": `kept-token:https://r.ex/ext/a\\b`} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			text := clierr.ExpandRemedies(keptTokenNotice(config.KeptCachedCred{Key: key, Ref: ref, Kind: config.CacheRecordServer, Server: "https://r.ex"}), "")
			assert.Contains(t, text, "rancher auth list-tokens")
			assert.NotContains(t, text, "auth delete-token")
			assert.Equal(t, 1, strings.Count(text, clierr.LeftOutNote), text)
		})
	}
	owner := clierr.ExpandRemedies(keptTokenNotice(config.KeptCachedCred{Key: "kept-token:https://r.ex/ext/a'b", Ref: ref, Kind: config.CacheRecordOwner, Server: "https://r.ex"}), "")
	assert.Contains(t, owner, "rancher auth list-tokens")
	assert.NotContains(t, owner, clierr.LeftOutNote)
}
