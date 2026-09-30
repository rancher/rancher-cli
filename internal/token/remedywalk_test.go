package token

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/remedytest"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// execPluginRemedyTexts is the text of every remedy the exec plugin can
// print, built by the code that prints it: its own errors and notices, and
// the shared ones it relays, including those that reach it through a
// builder whose output also fills other commands' result data.
func execPluginRemedyTexts(t *testing.T) map[string]string {
	t.Helper()
	cfg := &config.Config{
		CurrentServer: "prod",
		Servers: map[string]config.Server{
			"prod":             {URL: "https://r.example.com"},
			"staging":          {URL: "https://r.example.com"},
			"exec-login-r.ex":  {URL: "https://r.example.com", Provenance: clierr.ProvenanceExecLogin},
			"exec-login-r.ex2": {URL: "https://r.example.com", Provenance: clierr.ProvenanceExecLogin},
		},
	}
	hits := []storedCredential{{name: "prod", entry: cfg.Servers["prod"]}, {name: "staging", entry: cfg.Servers["staging"]}}
	texts := map[string]string{}
	add := func(name string, err error) {
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, name)
		if strings.Contains(ce.Message, "rancher") {
			texts[name+" message"] = ce.Message
		}
		if ce.Hint != "" {
			texts[name+" hint"] = ce.Hint
		}
	}
	add("several plugin login entries", pluginEntriesError("https://r.example.com", []string{"exec-login-r.ex", "exec-login-r.ex2"}, cfg))
	add("a tie", entryNotNamedError(cfg, hits, "c-1", false))
	add("a tie under the legacy shim", entryNotNamedError(cfg, hits, "c-1", true))
	add("an entry at another URL", entryOnOtherServerError("prod", "https://other.example.com", "https://r.example.com", "c-1", "prod", false))
	add("an entry at another URL with one here", entryOnOtherServerError("prod", "https://other.example.com", "https://r.example.com", "c-1", "staging", true))
	add("not logged in, blank", config.ErrNotLoggedInFor(nil))
	add("not logged in, one entry", config.ErrNotLoggedInFor(map[string]string{"prod": "https://r.example.com"}))
	add("not logged in, several entries", config.ErrNotLoggedInFor(map[string]string{"prod": "a", "staging": "b"}))

	dir := t.TempDir()
	nt := tokenCmd{dir: dir}
	add("no terminal", nt.withNoTerminalRemedies(interactiveLoginRequired("localProvider", "x"),
		noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: "https://r.example.com"}))
	add("no terminal, no --name", nt.withNoTerminalRemedies(interactiveLoginRequired("localProvider", "x"),
		noTerminalCall{clusterID: "c-1", serverURL: "https://r.example.com"}))

	for name, record := range map[string]string{
		"no terminal, a store change unfinished":        "version: 1\nid: 0123456789abcdef0123456789abcdef\nsteps:\n- kind: pin\n  target: file\n",
		"no terminal, a store change record unreadable": "not: [yaml",
	} {
		changed := filepath.Join(t.TempDir(), "rancher")
		require.NoError(t, os.MkdirAll(changed, 0o700))
		require.NoError(t, os.WriteFile(config.TransitionPath(changed), []byte(record), 0o600))
		c := &config.Config{CredentialStore: "file", CredentialTransitionID: "0123456789abcdef0123456789abcdef"}
		c.SetPath(config.FilePath(changed))
		require.NoError(t, c.Write())
		add(name, tokenCmd{dir: changed}.withNoTerminalRemedies(interactiveLoginRequired("localProvider", "x"),
			noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: "https://r.example.com"}))
	}

	rec := config.SupersededToken{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc"}}
	texts["a superseded token that was kept"] = keptWarning(rec, "https://r.example.com", "the server refused")
	text, hint := ResidualRecovery(clierr.TokenFate{Ref: &clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "token-v3"}}, "prod", "m", true)
	texts["a token left behind, stored"], texts["a token left behind, stored, hint"] = text, hint
	text, _ = ResidualRecovery(clierr.TokenFate{Ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "t"}}, "prod", "m", false)
	texts["a token left behind, not stored"] = text
	text, _ = ResidualRecovery(clierr.TokenFate{Marked: true}, "prod", "m", false)
	texts["a marked token of unknown kind"] = text
	text, _ = ResidualRecovery(clierr.TokenFate{Search: &clierr.UnmarkedTokenSearch{AuthProvider: "saml"}}, "prod", "m", true)
	texts["an unmarked session token"] = text
	texts["a sign-in again"], texts["a sign-in again, hint"] = reLogin("prod")
	texts["the regenerate steps"] = RegenerateSteps(clierr.Rancher, "prod", "c-1")
	texts["a login with no entry named"] = config.LoginCommand
	texts["a logout"] = config.LogoutHintFor("prod")
	texts["a server entry's removal"] = config.DeleteServerCommand + " " + clierr.ShellQuote("prod")
	texts["token reuse off"] = config.TokenReuseOffCommand
	texts["the status report"] = config.StatusCommand
	texts["a timed-out read"] = provider.InspectClusterRemedy
	texts["a refused request"] = provider.AuthStatusRemedy
	texts["a missing --cluster"] = engine.ClusterRequiredHint
	for _, kind := range []string{"cluster", "pod"} {
		k, ok := kinds.Lookup(kind)
		require.True(t, ok)
		add("a "+kind+" not found", kinds.NotFoundCurated(k, "x", "c-1"))
	}
	var rejected bytes.Buffer
	_ = tokenCmd{}.classifyRejection(errCredentialUnauthorized, storedCredential{name: "prod", entry: cfg.Servers["prod"]},
		ui.New(&rejected, nil, true, false))
	texts["a stored login rejected"] = strings.TrimSuffix(rejected.String(), "\n")
	st := LoginStates{Commit: &config.CommitOutcome{Backend: clierr.BackendKeyring, State: config.CommitPossiblyCommitted}}
	texts["the local credential"] = credentialRecoveryFor(st, "prod").Command
	return texts
}

// TestEveryExecPluginRemedyIsWrittenForTheInvocation asserts every command
// in the exec plugin's remedies opens with the marker - none is left
// plain for the invocation to miss - and that each one names the
// directory once expanded, for the default directory and any other.
func TestEveryExecPluginRemedyIsWrittenForTheInvocation(t *testing.T) {
	t.Parallel()
	texts := execPluginRemedyTexts(t)
	words, err := remedytest.DirectoryCommandWords("../cli/testdata/schema.golden.json")
	require.NoError(t, err)
	plainCommandRE := regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])rancher (?:` + words + `)(?:[^A-Za-z0-9_-]|$)`)
	for name, s := range texts {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.True(t, clierr.HasRemedy(s), "%q names no command", s)
			// The shim's tie names its own command; it runs nothing.
			left := strings.ReplaceAll(strings.ReplaceAll(s, clierr.Rancher, ""), "the legacy rancher token command", "")
			assert.NotRegexp(t, plainCommandRE, left, "a command written without the marker")
			for _, dir := range []string{"/home/u/.config/rancher", "/tmp/acct dir", `C:\Users\u\AppData\Roaming\rancher`} {
				got := runtime.IO{RemedyDir: dir}.ExpandText(s)
				want := "rancher --config " + clierr.ShellQuote(dir) + " "
				assert.Equal(t, strings.Count(s, clierr.Rancher), strings.Count(got, want), got)
				assert.NotContains(t, got, clierr.RemedyMarker)
			}
		})
	}
}

// ExecPluginRemedyTextsForTest hands execPluginRemedyTexts to the external
// test package, which runs each command through the real command tree.
func ExecPluginRemedyTextsForTest(t *testing.T) map[string]string {
	t.Helper()
	return execPluginRemedyTexts(t)
}
