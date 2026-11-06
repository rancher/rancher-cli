package auth

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// warningsInJSON renders warnings in the JSON envelope and returns the
// warnings decoded from it.
func warningsInJSON(t *testing.T, warnings []string) []string {
	t.Helper()
	env := output.NewEnvelope("LoginResult", map[string]any{})
	env.Warnings = warnings
	var buf bytes.Buffer
	require.NoError(t, output.Render(&buf, "json", env))
	var got struct {
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	return got.Warnings
}

// TestLoginKeptTokenWarningStripsThePriorOwner asserts the warning of a login
// over a token another user owns names that user without invisible or
// direction-changing runes in the JSON envelope, and names no user when the
// stored id holds only such runes.
func TestLoginKeptTokenWarningStripsThePriorOwner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, owner, want string }{
		{"stripped", "user-z\u202ex\u2028y", "a different user (user-zxy) owns it"},
		{"nothing left", "\u202e\u2063", "a different user owns it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := testrancher.New(testrancher.Options{Users: []testrancher.User{{Name: "a", Password: "pw", UserID: "user-a"}}})
			fake := httptest.NewServer(f)
			t.Cleanup(fake.Close)
			dir, store := cmdtest.FilePolicyDir(t)
			seedServer(t, dir, "prod", fake.URL, "", false)
			cfg, err := config.Load(config.FilePath(dir))
			require.NoError(t, err)
			s := cfg.Servers["prod"]
			s.Provenance, s.TokenRef, s.TokenUserID = clierr.ProvenanceDerived, &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-prior"}, tc.owner
			cfg.Servers["prod"] = s
			require.NoError(t, cfg.Write())

			res, err := newPlainLoginCmd(dir, store).Execute(t.Context(),
				cmdtest.Input{Str: map[string]string{"auth-provider": "localProvider"}, Pos: []string{"prod"}},
				ui.New(&bytes.Buffer{}, newLineReader("a", "pw"), false, true))
			require.NoError(t, err)
			var kept string
			for _, w := range warningsInJSON(t, res.Warnings) {
				if strings.Contains(w, "was not retired") {
					kept = w
				}
			}
			require.NotEmpty(t, kept, "a warning says the prior token was not retired")
			assert.Contains(t, kept, tc.want)
			assert.NotContains(t, kept, "\u202e")
			assert.NotContains(t, kept, "\u2028")
		})
	}
}
