package configcmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// unprintableTokenNames are token names a command cannot print so that every
// supported shell reads them as written.
var unprintableTokenNames = map[string]string{
	"zero width space":       "abc\u200b",
	"quote":                  "a'b",
	"right single quotation": "a\u2019b",
	"two backslashes":        `a\\b`,
}

// recordedTokenServer is an entry of the exec plugin whose current token and
// one superseded record carry name.
func recordedTokenServer(kind clierr.TokenKind, name string) config.Server {
	return config.Server{
		URL: "https://r.example", Provenance: clierr.ProvenanceExecLogin, TokenUserID: "u-1",
		TokenRef: &clierr.TokenRef{Kind: kind, Name: name},
		SupersededTokens: []config.SupersededToken{{
			Ref: clierr.TokenRef{Kind: kind, Name: name}, UserID: "u-1", SupersededAt: "2026-01-01T00:00:00Z",
		}},
	}
}

// listCommand is the command that lists the tokens of kind, as the json form
// of a left-out recovery names it.
func listCommand(kind clierr.TokenKind) string {
	if kind == clierr.TokenKindV3 {
		return "rancher get management-tokens"
	}
	return "rancher get tokens"
}

// TestConfigStatusLeavesOutTheDeleteOfATokenNameThatCannotBePrinted asserts
// the text of config status for a recorded token whose name cannot be printed
// in a command gives no delete line, gives the list command and the note, and
// that the json recovery is the list command alone.
func TestConfigStatusLeavesOutTheDeleteOfATokenNameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for caseName, name := range unprintableTokenNames {
		for _, kind := range []clierr.TokenKind{clierr.TokenKindExt, clierr.TokenKindV3} {
			t.Run(caseName+" "+string(kind), func(t *testing.T) {
				t.Parallel()
				s := recordedTokenServer(kind, name)

				var b strings.Builder
				renderFields(entrySection("e", s), "  ", &b)
				text := clierr.ExpandRemedies(b.String(), "")
				assert.NotContains(t, text, "delete token")
				assert.NotContains(t, text, "delete management-token")
				assert.Contains(t, text, listCommand(kind))
				assert.Contains(t, text, clierr.LeftOutNote)

				report := entryReport("e", s)
				current, ok := report["currentToken"].(map[string]any)
				require.True(t, ok)
				records, ok := report["supersededTokens"].([]map[string]any)
				require.True(t, ok)
				for _, recovery := range [][]string{current["recovery"].([]string), records[0]["recovery"].([]string)} {
					require.Len(t, recovery, 1)
					assert.Equal(t, listCommand(kind), clierr.ExpandRemedies(recovery[0], ""))
				}
			})
		}
	}
}

// TestSetTokenReuseLeavesOutTheDeleteOfATokenNameThatCannotBePrinted asserts
// the text of config set-token-reuse for a dormant plugin login that holds a
// token whose name cannot be printed in a command gives no delete line and
// gives the list command and the note.
func TestSetTokenReuseLeavesOutTheDeleteOfATokenNameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for caseName, name := range unprintableTokenNames {
		t.Run(caseName, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{Servers: map[string]config.Server{
				"exec-login-r": recordedTokenServer(clierr.TokenKindExt, name),
			}}
			_, lines := dormantPluginLogins(cfg)
			text := clierr.ExpandRemedies(strings.Join(lines, "\n"), "")
			assert.NotContains(t, text, "delete token")
			assert.Contains(t, text, "rancher get tokens")
			assert.Contains(t, text, clierr.LeftOutNote)
			assert.Contains(t, text, "rancher config delete-server 'exec-login-r'", "the entry's own commands stay")
		})
	}
}

// TestDeleteServerRemovalTextsLeaveOutTheCommandForAnEntryNameThatCannotBePrinted
// asserts the commands that name a server entry are left out, with the note
// once, when the entry name cannot be printed.
func TestDeleteServerRemovalTextsLeaveOutTheCommandForAnEntryNameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for caseName, name := range unprintableTokenNames {
		t.Run(caseName, func(t *testing.T) {
			t.Parallel()
			err := execLoginURLChangeRefused(name)
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.NotContains(t, ce.Message, "config delete-server")
			assert.NotContains(t, ce.Message, "auth logout")
			assert.Equal(t, 1, strings.Count(ce.Message, clierr.LeftOutNote))
			assert.Equal(t, "", deleteServerCommand(name).Field())
		})
	}
}
