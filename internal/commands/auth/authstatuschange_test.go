package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/ui"
)

const (
	unfinishedNote = "a credential store change is unfinished; run rancher config status to see it"
	unreadableNote = "the credential store change record cannot be read, so every auth login, auth logout, config delete-server and config update-server --url refuses until it is restored or moved aside; run rancher config status to see it"
)

// TestAuthStatusReportsTheCredentialStoreChange asserts auth status adds
// the same report config status gives when it succeeds, and that where it
// fails before its report its own error keeps its hint and its message
// names the unfinished change - once, whichever layer found it. While the
// change record cannot be read, named or not, the status report is the
// hint and the error's own remedy stays in its message.
func TestAuthStatusReportsTheCredentialStoreChange(t *testing.T) {
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCredentialStore, "")
	okPreflight := func(context.Context, config.Effective) (string, error) { return "v1.31.0", nil }
	loggedIn := func(t *testing.T) string {
		t.Helper()
		dir := seedAuthedDir(t)
		require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "default", URL: "https://rancher.test.invalid"}, "tok"))
		return dir
	}

	t.Run("success adds the rows", func(t *testing.T) {
		dir := loggedIn(t)
		cmdtest.WriteChangeRecord(t, dir, true, cmdtest.ChangeStep{Kind: "pin", Target: "file"})
		res, err := (&authCheckCmd{dir: dir, store: config.NewFileStore(dir), preflight: okPreflight}).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
		require.NoError(t, err)
		raw, err := json.Marshal(res.Data.(map[string]any)["credentialStoreChange"])
		require.NoError(t, err)
		var row map[string]any
		require.NoError(t, json.Unmarshal(raw, &row))
		assert.Equal(t, config.ChangeRecordActive, row["state"])
		assert.Equal(t, []any{map[string]any{"kind": "pin", "store": "file"}}, row["remaining"])
		assert.Contains(t, res.Text, "\nCREDENTIAL STORE CHANGE\n  state: active - ")
		assert.Contains(t, res.Text, "\n  remaining:\n    switch the pin to file - not tried yet\n")
		assert.Contains(t, styleAuthCheckText(res.Text, true, style.Style{Enabled: true}), "\x1b[1mCREDENTIAL STORE CHANGE\x1b[0m\n",
			"the section header is bold, like the others")
	})

	t.Run("success with nothing recorded adds none", func(t *testing.T) {
		dir := loggedIn(t)
		res, err := (&authCheckCmd{dir: dir, store: config.NewFileStore(dir), preflight: okPreflight}).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
		require.NoError(t, err)
		assert.NotContains(t, res.Data.(map[string]any), "credentialStoreChange")
		assert.NotContains(t, res.Text, "CREDENTIAL STORE CHANGE")
	})

	for _, tc := range []struct {
		name      string
		setup     func(t *testing.T, dir string)
		preflight func(context.Context, config.Effective) (string, error)
		// code and hint are the error's own; empty code is an error the
		// store returns unclassified.
		code string
		hint string
	}{
		{name: "not logged in, found by the connection", preflight: provider.Preflight,
			code: clierr.CodeNotLoggedIn, hint: "rancher auth login 'default'"},
		{name: "no current server", code: clierr.CodeNotLoggedIn, hint: "rancher auth login 'default'",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				cfg, err := config.Load(config.FilePath(dir))
				require.NoError(t, err)
				cfg.CurrentServer = ""
				require.NoError(t, cfg.Write())
			}},
		{name: "the credential cannot be read",
			setup: func(t *testing.T, dir string) {
				t.Helper()
				require.NoError(t, os.WriteFile(config.CredentialsFilePath(dir), []byte("not: [yaml"), 0o600))
			}},
		{name: "the server refuses", code: clierr.CodeAuth, hint: "rancher auth login 'default'",
			preflight: func(context.Context, config.Effective) (string, error) {
				return "", clierr.New(clierr.CodeAuth, "login rejected").WithHint("rancher auth login 'default'")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, record := range []string{"none", "in flight", "damaged, named", "damaged, named by nothing", "never started"} {
				t.Run(record, func(t *testing.T) {
					dir := seedAuthedDir(t)
					if tc.setup != nil {
						tc.setup(t, dir)
					}
					switch record {
					case "in flight":
						cmdtest.WriteChangeRecord(t, dir, true, cmdtest.ChangeStep{Kind: "pin", Target: "file"})
					case "damaged, named":
						cmdtest.WriteDamagedChangeRecord(t, dir, true)
					case "damaged, named by nothing":
						cmdtest.WriteDamagedChangeRecord(t, dir, false)
					case "never started":
						cmdtest.WriteChangeRecord(t, dir, false, cmdtest.ChangeStep{Kind: "pin", Target: "file"})
					}
					pf := tc.preflight
					if pf == nil {
						pf = okPreflight
					}
					_, err := (&authCheckCmd{dir: dir, store: config.NewFileStore(dir), preflight: pf}).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
					require.Error(t, err)
					note, hint := unfinishedNote, tc.hint
					switch record {
					case "none", "never started":
						note = ""
					case "damaged, named", "damaged, named by nothing":
						// The login or logout the hint names is refused on
						// the record; the report is the hint instead, and
						// the remedy stays in the message.
						note, hint = unreadableNote, "rancher config status"
					}
					damaged := strings.HasPrefix(record, "damaged")
					if tc.code != "" || damaged {
						ce, ok := errors.AsType[*clierr.Error](err)
						require.True(t, ok)
						if tc.code != "" {
							assert.Equal(t, tc.code, ce.Code)
						}
						assert.Equal(t, hint, ce.Hint)
						if damaged && tc.hint != "" {
							assert.Contains(t, ce.Message, tc.hint, "the error's own remedy stays in the message")
						}
					}
					if note == "" {
						assert.NotContains(t, err.Error(), "config status")
					} else {
						assert.Equal(t, 1, strings.Count(err.Error(), note), "the message names the change, once")
					}
				})
			}
		})
	}
}
