package token

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// TestTheTextsOfAnUnusableEntryLeaveOutTheDeleteOfATokenNameThatCannotBePrinted
// asserts that the warning of a token recorded on an entry whose URL cannot be
// used, which says the token stays on the server that issued it and how to
// delete it there, gives no delete command for a name that cannot be printed
// exactly, gives the command that lists the tokens, and holds the note once.
func TestTheTextsOfAnUnusableEntryLeaveOutTheDeleteOfATokenNameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	const url = "https://r.example.com"
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ref := TokenRef{Kind: TokenKindExt, Name: tc.value}
			cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: url + "/p@x"}}}
			d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{}}
			it := &revocationItem{source: sourceCache, ref: &ref, server: config.CanonicalServerIdentity(url), owner: "u-abc12", entry: "prod", reason: reasonFailed}
			assertDeleteLeftOut(t, d.manualWarning(it), TokenKindExt)
			assertDeleteLeftOut(t, config.TokenStaysText(ref, "u-abc12"), TokenKindExt)
		})
	}
}

// TestTheRefusalOfAKeyThatMatchesSeveralRecordsLeavesOutTheDeleteOfANameThatCannotBePrinted
// asserts that the refusal of a key that selects several records leaves out
// the delete-token command for a record name that cannot be printed exactly,
// keeps its facts, and holds the note once.
func TestTheRefusalOfAKeyThatMatchesSeveralRecordsLeavesOutTheDeleteOfANameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := clierr.ExpandRemedies(maskedKeyConflict(maskedMatch{n: 2, name: tc.value}).Message, "")
			assert.Contains(t, msg, "the key matches 2 cached credentials")
			assert.NotContains(t, msg, "auth delete-token")
			assert.Equal(t, 1, strings.Count(msg, clierr.LeftOutNote), msg)
		})
	}
}

// TestTheRegenerateRouteOfAnUnusableEntryIsLeftOutForAClusterThatCannotBePrinted
// asserts that the refusal of a call that names an entry whose URL cannot be
// used, when a usable entry is registered at the call's server, leaves out
// the whole route that selects that entry and regenerates the kubeconfig when
// the cluster id cannot be printed exactly, and holds the note once.
func TestTheRegenerateRouteOfAnUnusableEntryIsLeftOutForAClusterThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	const server = "https://r.example.com"
	cfg := &config.Config{Servers: map[string]config.Server{"x": {URL: server + "/p@x"}, "y": {URL: server}}}
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := unusableEntryError(cfg, "x", server, tc.value, true)
			require.NotNil(t, err)
			msg := clierr.ExpandRemedies(err.Message, "")
			assert.NotContains(t, msg, "config use-server")
			assert.Equal(t, 1, strings.Count(msg, clierr.LeftOutNote), msg)
		})
	}
}
