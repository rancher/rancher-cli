package token

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// keptOwnerCases are the stored owner ids of a kept superseded token and the
// part of the warning that names the owner after the strip.
var keptOwnerCases = []struct{ name, owner, want string }{
	{"stripped", "user-z\u202ex\u2028y", "a different user (user-zxy) owns it"},
	{"nothing left", "\u202e\u2063", "a different user owns it"},
}

// keptWarningInJSON is the one warning that says a superseded token was not
// retired, decoded from the JSON envelope the warnings render in.
func keptWarningInJSON(t *testing.T, warnings []string) string {
	t.Helper()
	env := output.NewEnvelope("LoginResult", map[string]any{})
	env.Warnings = warnings
	var kept []string
	for _, w := range renderAndRead(t, "json", env).Warnings {
		if strings.Contains(w, "was not retired") {
			kept = append(kept, w)
		}
	}
	require.Len(t, kept, 1)
	return kept[0]
}

// TestRetireKeptWarningStripsTheOtherOwner asserts the warning for a record
// another user owns names that user without direction-changing, invisible or
// separator runes in the JSON envelope, and names no user when the stored id
// holds only such runes.
func TestRetireKeptWarningStripsTheOtherOwner(t *testing.T) {
	for _, tc := range keptOwnerCases {
		t.Run(tc.name, func(t *testing.T) {
			x := newRetireFixture(t, testrancher.Options{})
			other, fresh := x.credAs(t, "b"), x.credAs(t, "a")
			rec := record(other, "2026-09-02T00:00:00Z")
			rec.UserID = tc.owner
			x.seedRecords(t, []config.SupersededToken{rec})

			rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, fresh, x.records(t))
			kept := keptWarningInJSON(t, rep.Warnings)
			assert.Contains(t, kept, tc.want)
			assert.NotContains(t, kept, "\u202e")
			assert.NotContains(t, kept, "\u2028")
		})
	}
}

// TestPluginLoginKeptWarningStripsTheOtherOwner asserts the same for the
// warnings of a plugin-created login that replaces an expired one: the
// record of the prior token holds an owner id with such runes.
func TestPluginLoginKeptWarningStripsTheOtherOwner(t *testing.T) {
	for _, tc := range keptOwnerCases {
		t.Run(tc.name, func(t *testing.T) {
			x := newPluginFixture(t, true, testrancher.Options{})
			_, err := x.getToken(t, "c-test", "n", localPrompter())
			require.NoError(t, err)
			before, _ := x.entry(t)
			require.NoError(t, config.UpdateServerEntry(t.Context(), x.dir, x.entryName(), func(s *config.Server) bool {
				s.TokenUserID = tc.owner
				return true
			}))
			require.NoError(t, x.store.SetToken(config.ServerRef{Name: x.entryName(), URL: x.url}, "ext/"+before.TokenRef.Name+":wrong"))

			res, err := x.getToken(t, "testcluster", "n2", localPrompter())
			require.NoError(t, err)
			kept := keptWarningInJSON(t, res.Warnings)
			assert.Contains(t, kept, tc.want)
			assert.NotContains(t, kept, "\u202e")
			assert.NotContains(t, kept, "\u2028")
		})
	}
}
