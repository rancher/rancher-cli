package configcmd

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// runJSON runs cmd through the pipeline as json for an invocation whose
// remedy commands name remedyDir (none when it is empty), and returns the
// decoded data of the envelope.
func runJSON(t *testing.T, cmd runtime.Command, in cmdtest.Input, remedyDir string) map[string]any {
	t.Helper()
	var out, errBuf bytes.Buffer
	_, ce := runtime.Run(t.Context(), cmd, in, runtime.IO{Out: &out, Err: &errBuf, Format: "json", RemedyDir: remedyDir}, safety.Binding{})
	require.Nil(t, ce)
	require.True(t, json.Valid(out.Bytes()), "%q", out.Bytes())
	var doc struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &doc))
	return doc.Data
}

// recoveryLists returns every list under a key named recovery in decoded
// data, in the order of the document with the keys of each object sorted.
func recoveryLists(t *testing.T, v any) [][]string {
	t.Helper()
	var lists [][]string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				if entries, ok := x[k].([]any); ok && k == "recovery" {
					var list []string
					for _, e := range entries {
						s, ok := e.(string)
						require.True(t, ok, "recovery entry %v is not a string", e)
						list = append(list, s)
					}
					lists = append(lists, list)
					continue
				}
				walk(x[k])
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(v)
	return lists
}

// withDirectory is the command with prefix written after its first word.
func withDirectory(prefix, command string) string {
	return strings.Replace(command, "rancher", "rancher"+prefix, 1)
}

// extRecoveryConfig holds a plugin login entry with a current token and an
// extension token record, whose recovery is a login sequence.
func extRecoveryConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{TokenReuse: true, Servers: map[string]config.Server{
		"exec-login-r.ex": {URL: "https://r.ex", Provenance: clierr.ProvenanceExecLogin,
			TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-live"}, TokenUserID: "user-a",
			SupersededTokens: []config.SupersededToken{{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-old"}, UserID: "user-b", SupersededAt: "2026-09-01T00:00:00Z"}}},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir
}

// TestTokenRecoveryCommandsNameTheConfigDirectoryInJSON asserts, for config
// status and for config set-token-reuse off, with the v3 delete and with the
// login sequence of an extension token, that every command of each recovery
// list in the json data names the invocation's config directory after the
// word rancher, and is the command the run with no directory writes, with no
// character of the remedy marker.
func TestTokenRecoveryCommandsNameTheConfigDirectoryInJSON(t *testing.T) {
	remedyDir := t.TempDir()
	prefix, printable := runtime.RemedyPrefix(remedyDir)
	require.True(t, printable)

	for _, tc := range []struct {
		name   string
		config func(t *testing.T) string
		cmd    func(dir string) runtime.Command
		in     cmdtest.Input
	}{
		{"config status, a v3 token", v3RecoveryConfig, NewConfigInfo, cmdtest.Input{}},
		{"config status, an extension token", extRecoveryConfig, NewConfigInfo, cmdtest.Input{}},
		{"config set-token-reuse off, a v3 token", v3RecoveryConfig, NewSetTokenReuse, cmdtest.Input{Pos: []string{"off"}}},
		{"config set-token-reuse off, an extension token", extRecoveryConfig, NewSetTokenReuse, cmdtest.Input{Pos: []string{"off"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(config.EnvCredentialStore, "")
			plainDir, namedDir := tc.config(t), tc.config(t)
			plain := recoveryLists(t, runJSON(t, tc.cmd(plainDir), tc.in, ""))
			named := recoveryLists(t, runJSON(t, tc.cmd(namedDir), tc.in, remedyDir))

			require.NotEmpty(t, plain, "the run has a recovery list")
			require.Len(t, named, len(plain))
			commands := 0
			for i, list := range plain {
				require.Len(t, named[i], len(list))
				for j, command := range list {
					commands++
					assert.True(t, strings.HasPrefix(named[i][j], "rancher"+prefix+" "), "%q", named[i][j])
					assert.Equal(t, withDirectory(prefix, command), named[i][j])
					assert.False(t, strings.ContainsFunc(named[i][j], func(r rune) bool { return r >= '\u2060' && r <= '\u2064' }), "%q", named[i][j])
				}
			}
			assert.Positive(t, commands, "the run has a recovery command")
		})
	}
}

// TestCredentialStoreRemedyInJSONNamesTheConfigDirectory asserts the
// credentialStoreError of config status and the note of config set-completion
// with --network true, for a config directory with credentials in both
// stores and no pin, name the invocation's config directory in the command
// that logs out of the entry, with no character of the remedy marker.
func TestCredentialStoreRemedyInJSONNamesTheConfigDirectory(t *testing.T) {
	keyring.MockInit()
	t.Setenv(config.EnvCredentialStore, "")
	t.Setenv(config.EnvToken, "")
	remedyDir := t.TempDir()
	prefix, printable := runtime.RemedyPrefix(remedyDir)
	require.True(t, printable)
	want := "run: rancher" + prefix + " auth logout 'staging'"

	for _, tc := range []struct {
		name string
		key  string
		cmd  func(dir string) runtime.Command
		in   cmdtest.Input
	}{
		{"config status", "credentialStoreError", NewConfigInfo, cmdtest.Input{}},
		{"config set-completion", "note", NewSetCompletion, cmdtest.Input{Str: map[string]string{"network": "true"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cmdtest.SeedUnpinned(t, dir, map[string]string{"staging": "b"}, map[string]string{"prod": "a"})
			data := runJSON(t, tc.cmd(dir), tc.in, remedyDir)
			got, ok := data[tc.key].(string)
			require.True(t, ok, "%s is a string in %v", tc.key, data)
			assert.Contains(t, got, want)
			assert.False(t, strings.ContainsFunc(got, func(r rune) bool { return r >= '\u2060' && r <= '\u2064' }), "%q", got)
		})
	}
}
