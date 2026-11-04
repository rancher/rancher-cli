package token

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
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
	pluginNames := []string{"exec-login-r.ex", "exec-login-r.ex2"}
	owner := userCheck{userID: "u-abc12", clusterID: "c-1", serverID: "https://r.example.com", caSource: "/etc/rancher/ca.pem"}
	add("several plugin login entries", pluginEntriesError("https://r.example.com", pluginNames, cfg, "", userCheck{}))
	add("several plugin login entries of one user", pluginEntriesError("https://r.example.com", pluginNames, cfg, "prod", owner))
	add("several plugin login entries of one user, no entry named", pluginEntriesError("https://r.example.com", pluginNames, cfg, "", owner))
	add("a tie", entryNotNamedError(cfg, hits, "c-1", false))
	add("a tie under the legacy shim", entryNotNamedError(cfg, hits, "c-1", true))
	elsewhere := func(current string, here bool, extra map[string]config.Server) *config.Config {
		c := &config.Config{CurrentServer: current, Servers: map[string]config.Server{"prod": {URL: "https://other.example.com"}}}
		if here {
			c.Servers["staging"] = config.Server{URL: "https://r.example.com"}
		}
		maps.Copy(c.Servers, extra)
		return c
	}
	add("an entry at another URL", entryOnOtherServerError(elsewhere("prod", false, nil), "prod", "https://r.example.com", "c-1"))
	add("an entry at another URL with one here", entryOnOtherServerError(elsewhere("staging", true, nil), "prod", "https://r.example.com", "c-1"))
	plugin := map[string]config.Server{"exec-login-o": {URL: "https://other.example.com", CA: "/etc/rancher/other-ca.pem", Provenance: clierr.ProvenanceExecLogin}}
	add("a plugin login entry at another URL", entryOnOtherServerError(elsewhere("prod", false, plugin), "exec-login-o", "https://r.example.com", "c-1"))
	add("a plugin login entry at another URL with one here", entryOnOtherServerError(elsewhere("staging", true, plugin), "exec-login-o", "https://r.example.com", "c-1"))
	add("not logged in, blank", config.ErrNotLoggedInFor(nil, nil))
	add("not logged in, one entry", config.ErrNotLoggedInFor(map[string]string{"prod": "https://r.example.com"}, nil))
	add("not logged in, several entries", config.ErrNotLoggedInFor(map[string]string{"prod": "a", "staging": "b"}, nil))
	add("not logged in, the one entry a plugin login entry", config.ErrNotLoggedInFor(map[string]string{"exec-login-r.ex": "https://[2001:db8::1]:8443"}, nil))

	dir := t.TempDir()
	nt := tokenCmd{dir: dir}
	add("no terminal", nt.withNoTerminalRemedies(interactiveLoginRequired("local", "x"),
		noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: "https://r.example.com"}))
	add("no terminal, no --name", nt.withNoTerminalRemedies(interactiveLoginRequired("local", "x"),
		noTerminalCall{clusterID: "c-1", serverURL: "https://r.example.com"}))
	choice := clierr.New(clierr.CodeAmbiguousSelection, "x").WithCause(&providerChoiceNeedsTerminal{
		configs: []string{"local", "okta"}, noTerminal: []clierr.Candidate{{ID: "okta", Name: "okta"}}})
	add("no terminal, a provider choice", nt.withNoTerminalRemedies(choice,
		noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: "https://r.example.com", keepsLogin: true}))
	none := clierr.New(clierr.CodeAmbiguousSelection, "x").WithCause(&providerChoiceNeedsTerminal{
		configs: []string{"local", "openldap"}})
	add("no terminal, a provider choice no provider can answer", nt.withNoTerminalRemedies(none,
		noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: "https://r.example.com", keepsLogin: true}))

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
		add(name, tokenCmd{dir: changed}.withNoTerminalRemedies(interactiveLoginRequired("local", "x"),
			noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: "https://r.example.com"}))
	}

	// A directory whose current server holds the user's login at the
	// call's server: create kubeconfig is offered.
	logged := t.TempDir()
	lc := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: "https://r.example.com", TokenUserID: "u-abc12"}}}
	lc.SetPath(config.FilePath(logged))
	require.NoError(t, lc.Write())
	require.NoError(t, config.NewFileStore(logged).SetToken(config.ServerRef{Name: "prod", URL: "https://r.example.com"}, "id:secret"))
	add("no terminal, the current server's login serves the call", tokenCmd{dir: logged, store: config.NewFileStore(logged)}.withNoTerminalRemedies(
		interactiveLoginRequired("local", "x"), noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: "https://r.example.com", userID: "u-abc12"}))
	add("no terminal, a --name of a plugin login entry", tokenCmd{dir: logged, store: config.NewFileStore(logged)}.withNoTerminalRemedies(
		interactiveLoginRequired("local", "x"), noTerminalCall{name: "exec-login-r.ex-c-1", clusterID: "c-1", serverURL: "https://r.example.com", caCert: "/etc/rancher/ca.pem"}))
	add("no terminal, --user-id without --name", nt.withNoTerminalRemedies(interactiveLoginRequired("local", "x"),
		noTerminalCall{clusterID: "c-1", serverURL: "https://r.example.com", userID: "u-abc12", caCert: "/etc/rancher/ca.pem", keepsLogin: true}))
	add("no terminal, --user-id without --name, token reuse off", nt.withNoTerminalRemedies(interactiveLoginRequired("local", "x"),
		noTerminalCall{clusterID: "c-1", serverURL: "https://r.example.com", userID: "u-abc12"}))
	add("no terminal, --user-id without --cluster", nt.withNoTerminalRemedies(interactiveLoginRequired("local", "x"),
		noTerminalCall{serverURL: "https://r.example.com", userID: "u-abc12", keepsLogin: true}))
	add("no terminal, --user-id without --cluster, token reuse off", tokenCmd{dir: logged, store: config.NewFileStore(logged)}.withNoTerminalRemedies(
		interactiveLoginRequired("local", "x"), noTerminalCall{serverURL: "https://r.example.com", userID: "u-abc12"}))
	add("no terminal, a provider choice, --user-id without --name", nt.withNoTerminalRemedies(choice,
		noTerminalCall{clusterID: "c-1", serverURL: "https://r.example.com", userID: "u-abc12", keepsLogin: true}))
	add("no terminal, with --user-id", nt.withNoTerminalRemedies(interactiveLoginRequired("local", "x"),
		noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: "https://r.example.com", userID: "u-abc12"}))
	add("no terminal, with --user-id and an auth config", nt.withNoTerminalRemedies(interactiveLoginRequired("local", "x"),
		noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: "https://r.example.com", userID: "u-abc12", authConfig: "corp-ldap"}))
	add("no terminal, --user-id and an auth config without --name", nt.withNoTerminalRemedies(interactiveLoginRequired("local", "x"),
		noTerminalCall{clusterID: "c-1", serverURL: "https://r.example.com", userID: "u-abc12", authConfig: "corp-ldap", caCert: "/etc/rancher/ca.pem", keepsLogin: true}))
	add("no terminal, the current server's login serves the call through the auth config", tokenCmd{dir: logged, store: config.NewFileStore(logged)}.withNoTerminalRemedies(
		interactiveLoginRequired("local", "x"), noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: "https://r.example.com", userID: "u-abc12", authConfig: "corp-ldap"}))
	add("a plugin login entry name too long", userCheck{userID: "u-abc12", clusterID: "c-1", serverID: "https://r.example.com", caSource: "/etc/rancher/ca.pem"}.entryNameTooLong(cfg, config.MaxEntryNameLength+1))
	add("a plugin login entry name too long, with an auth config", userCheck{userID: "u-abc12", authConfig: "corp-ldap", clusterID: "c-1", serverID: "https://r.example.com", caSource: "/etc/rancher/ca.pem"}.entryNameTooLong(cfg, config.MaxEntryNameLength+1))
	add("a plugin login entry named from a server URL host with an invisible character", userCheck{userID: "u-abc12", clusterID: "c-1", serverID: "https://r.example.com", caSource: "/etc/rancher/ca.pem"}.entryNameCharacters(cfg, config.NameProblem{Rune: rune(0x200b)}))
	add("a plugin login entry named from a server URL host with an invisible character, with an auth config", userCheck{userID: "u-abc12", authConfig: "corp-ldap", clusterID: "c-1", serverID: "https://r.example.com", caSource: "/etc/rancher/ca.pem"}.entryNameCharacters(cfg, config.NameProblem{Rune: rune(0x200b)}))
	inlineCA := "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n"
	add("a plugin login entry named from a server URL host with an invisible character, an inline CA of the exec entry", userCheck{userID: "u-abc12", clusterID: "c-1", serverID: "https://r.example.com", caSource: inlineCA}.entryNameCharacters(cfg, config.NameProblem{Rune: rune(0x200b)}))
	add("a plugin login entry named from a server URL host with an invisible character, an inline CA of a server entry", userCheck{userID: "u-abc12", clusterID: "c-1", serverID: "https://r.example.com", caSource: inlineCA, caFrom: config.CASource{Kind: config.CASourceEntry, Entry: "prod"}}.entryNameCharacters(cfg, config.NameProblem{Rune: rune(0x200b)}))
	add("several plugin login entries of one user and an auth config", pluginEntriesError("https://r.example.com", pluginNames, cfg, "prod", userCheck{userID: "u-abc12", authConfig: "corp-ldap", clusterID: "c-1", serverID: "https://r.example.com"}))
	add("several plugin login entries of one user and an auth config, no entry named", pluginEntriesError("https://r.example.com", pluginNames, cfg, "", userCheck{userID: "u-abc12", authConfig: "corp-ldap", clusterID: "c-1", serverID: "https://r.example.com"}))
	for name, ca := range map[string]string{"": "", ", a CA path": "/etc/rancher/ca.pem", ", an inline CA": "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n"} {
		u := userCheck{userID: "u-abc12", clusterID: "c-1", serverID: "https://r.example.com", caSource: ca}
		add("another owner's login in a user entry"+name, u.entryOwnerRefused(cfg, "prod", "u-other", false, true))
		add("another owner's plugin login entry"+name, u.entryOwnerRefused(cfg, "exec-login-r.ex", "u-other", true, true))
		add("another owner recorded by a plugin login entry"+name, u.entryOwnerRefused(cfg, "exec-login-r.ex", "u-other", true, false))
		created := CreateResult{Outcome: CreateKnown, Kind: TokenKindExt, Ref: &TokenRef{Kind: TokenKindExt, Name: "token-abc"}, UserID: "u-other"}
		gone := &freshToken{create: created, cleanup: func(context.Context) RevokeOutcome { return RevokeOutcome{Result: ResultFailed, Error: "500"} }}
		add("a created token of another owner"+name, u.checkCreatedOwner(t.Context(), cfg, created, gone,
			storedCredential{name: "prod", entry: cfg.Servers["prod"]}))
		add("a created token of another owner, from a plugin login entry"+name, u.checkCreatedOwner(t.Context(), cfg, created, gone,
			storedCredential{name: "exec-login-r.ex", entry: cfg.Servers["exec-login-r.ex"]}))
		through := userCheck{userID: "u-abc12", authConfig: "corp-ldap", clusterID: "c-1", serverID: "https://r.example.com", caSource: ca}
		add("another owner's login in a user entry, through an auth config"+name, through.entryOwnerRefused(cfg, "prod", "u-other", false, true))
		add("another owner's plugin login entry, through an auth config"+name, through.entryOwnerRefused(cfg, "exec-login-r.ex", "u-other", true, true))
		add("another auth config's login in a user entry"+name, through.entryAuthConfigRefused(cfg, "prod", "openldap", false, true))
		add("another auth config's plugin login entry"+name, through.entryAuthConfigRefused(cfg, "exec-login-r.ex", "openldap", true, true))
		add("another auth config recorded by a plugin login entry"+name, through.entryAuthConfigRefused(cfg, "exec-login-r.ex", "openldap", true, false))
		other := CreateResult{Outcome: CreateKnown, Kind: TokenKindExt, Ref: &TokenRef{Kind: TokenKindExt, Name: "token-abc"}, UserID: "u-abc12", AuthConfig: "openldap"}
		otherGone := &freshToken{create: other, cleanup: func(context.Context) RevokeOutcome { return RevokeOutcome{Result: ResultFailed, Error: "500"} }}
		add("a created token of another auth config"+name, through.refuseOtherAuthConfig(t.Context(), cfg, other, otherGone, storedCredential{name: "prod", entry: cfg.Servers["prod"]}))
		add("a created token of another auth config, from a plugin login entry"+name, through.refuseOtherAuthConfig(t.Context(), cfg, other, otherGone,
			storedCredential{name: "exec-login-r.ex", entry: cfg.Servers["exec-login-r.ex"]}))
		add("a created token that names no auth config"+name, through.refuseNoAuthConfig(t.Context(), other, otherGone, storedCredential{name: "prod", entry: cfg.Servers["prod"]}))
		unscoped := userCheck{userID: "u-abc12", serverID: "https://r.example.com", caSource: ca}
		add("another owner's login in a user entry, no cluster"+name, unscoped.entryOwnerRefused(cfg, "prod", "u-other", false, true))
		add("another owner's plugin login entry, no cluster"+name, unscoped.entryOwnerRefused(cfg, "exec-login-r.ex", "u-other", true, true))
	}

	rec := config.SupersededToken{Ref: clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc"}}
	texts["a superseded token that was kept"] = keptWarning(rec, "https://r.example.com", "the server refused")
	text, hint := ResidualRecovery(clierr.TokenFate{Ref: &clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "token-v3"}}, "prod", "m", true)
	require.Empty(t, hint, "the administrator's delete is not valid verbatim for every caller")
	texts["a v3 token left behind, stored"] = text
	text, hint = ResidualRecovery(clierr.TokenFate{Ref: &clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "token-v3"}}, "prod", "m", false)
	require.Empty(t, hint)
	texts["a v3 token left behind, not stored"] = text
	text, hint = ResidualRecovery(clierr.TokenFate{Ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-ext"}}, "prod", "m", true)
	texts["a token left behind, stored"], texts["a token left behind, stored, hint"] = text, hint
	text, _ = ResidualRecovery(clierr.TokenFate{Ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "t"}}, "prod", "m", false)
	texts["a token left behind, not stored"] = text
	text, _ = ResidualRecovery(clierr.TokenFate{Marked: true}, "prod", "m", false)
	texts["a marked token of unknown kind"] = text
	text, _ = ResidualRecovery(clierr.TokenFate{Search: &clierr.UnmarkedTokenSearch{AuthProvider: "saml"}}, "prod", "m", true)
	texts["an unmarked session token"] = text
	for name, f := range signInLeftoverFates() {
		texts["a sign-in token left behind, "+name+", user known"] = signInLeftover(f, "u-abc12", "m")
		texts["a sign-in token left behind, "+name+", user unknown"] = signInLeftover(f, "", "m")
	}
	texts["a sign-in again"], texts["a sign-in again, hint"] = reLogin("prod")
	texts["the regenerate steps"] = RegenerateSteps(clierr.Rancher, "prod", "c-1")
	texts["a login with no entry named"] = config.LoginCommand
	texts["a logout"] = config.LogoutHintFor("prod")
	texts["a server entry's removal"] = config.DeleteServerCommand + clierr.ShellQuoteArgs("", "prod")
	texts["token reuse off"] = config.TokenReuseOffCommand
	texts["the status report"] = config.StatusCommand
	texts["a timed-out read"] = provider.InspectClusterRemedyFor("c-1")
	texts["a refused request"] = clierr.AuthStatusRemedy
	texts["a missing --cluster"] = engine.ClusterRequiredHint
	for _, kind := range []string{"cluster", "pod"} {
		k, ok := kinds.Lookup(kind)
		require.True(t, ok)
		add("a "+kind+" not found", kinds.NotFoundCurated(k, "x", "c-1"))
	}
	var rejected bytes.Buffer
	_ = tokenCmd{}.classifyRejection(errCredentialUnauthorized, storedCredential{name: "prod", entry: cfg.Servers["prod"]}, "", true,
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
			left := strings.ReplaceAll(s, clierr.Rancher, "")
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

// AssertCommandsEndTheirSentenceForTest hands assertCommandsEndTheirSentence
// to the external test package, which gives it the text of a remedy as an
// invocation with a config directory prints it.
func AssertCommandsEndTheirSentenceForTest(t *testing.T, text string) {
	t.Helper()
	assertCommandsEndTheirSentence(t, text)
}

// DashedNameRemediesForTest is, for each text a builder writes about a name
// that starts with a dash, the command paths its commands must reach in order,
// for the external test package, which runs each command through the real
// command tree. Every text comes from the production builder that writes it.
func DashedNameRemediesForTest(t *testing.T) map[string][]string {
	t.Helper()
	const dashed, url = "-x", "https://r.example.com"
	ext := TokenRef{Kind: TokenKindExt, Name: dashed}
	v3 := TokenRef{Kind: TokenKindV3, Name: dashed}
	_, deleteToken := clierr.RecoveryCommands(ext)
	_, deleteManagementToken := clierr.RecoveryCommands(v3)

	owned := &config.Config{Servers: map[string]config.Server{dashed: {URL: url, TokenUserID: "u-abc12"}}}
	item := &revocationItem{source: sourceCache, ref: &ext, server: config.CanonicalServerIdentity(url), owner: "u-abc12", entry: dashed, trust: dashed,
		reason: reasonFailed, tlsErr: clierr.New(clierr.CodeTLSTrust, "x"), cert: &tlstrust.Details{Kind: tlstrust.KindUnknownAuthority}}
	d := &Deletion{cfg: owned, view: owned, removed: map[string]bool{}, clients: map[string]revokeClient{dashed: {}}, items: []*revocationItem{item}}
	warnings := d.Warnings()
	require.Len(t, warnings, 1)

	u := userCheck{userID: "u-abc12", clusterID: "c-1", serverID: url}
	relogin, _ := u.relogin(owned, dashed, true)

	return map[string][]string{
		config.LoginHintFor(dashed):  {"auth login"},
		config.LogoutHintFor(dashed): {"auth logout"},
		useServerCommand(dashed):     {"config use-server"},
		d.revokeTLSRemedy(item).CA:   {"config update-server"},
		warnings[0]:                  {"auth login", "config update-server"},
		relogin:                      {"auth logout", "config delete-server", "auth login"},
		signInLeftover(clierr.TokenFate{Ref: &ext}, "u-abc12", "m"): {"delete token"},
		deleteToken:           {"delete token"},
		deleteManagementToken: {"delete management-token"},
		v3AdminRemedy(v3, ""): {"delete management-token"},
	}
}

// ExecPluginRemedyTextsForTest hands execPluginRemedyTexts to the external
// test package, which runs each command through the real command tree.
func ExecPluginRemedyTextsForTest(t *testing.T) map[string]string {
	t.Helper()
	return execPluginRemedyTexts(t)
}

// TestPluginLoginEntryRemediesNeverLogInAndRemoveOnlyALoginThatCannotAnswer
// asserts no remedy names auth login on a plugin login entry, whose name
// auth login refuses, and none names auth logout or config delete-server
// on one unless it is the refusal of a call whose --name identifies that
// entry (or that chose it by its recorded owner) when the entry holds
// another user's login, or one made through another auth config than the
// call's: the entry cannot answer the call, and removing it is the way to
// the call's own. Every other remedy leaves a plugin login entry alone,
// since it may be another user's login that their kubeconfigs use.
func TestPluginLoginEntryRemediesNeverLogInAndRemoveOnlyALoginThatCannotAnswer(t *testing.T) {
	t.Parallel()
	loginRE := regexp.MustCompile(`rancher auth login '?` + regexp.QuoteMeta(config.ExecLoginPrefix))
	removeRE := regexp.MustCompile(`rancher (?:auth logout|config delete-server) '?` + regexp.QuoteMeta(config.ExecLoginPrefix))
	removes := func(name string) bool {
		return strings.HasPrefix(name, "another owner") || strings.HasPrefix(name, "another auth config") ||
			strings.HasPrefix(name, "a created token of another owner, from a plugin login entry") ||
			strings.HasPrefix(name, "a created token of another auth config, from a plugin login entry")
	}
	for name, s := range execPluginRemedyTexts(t) {
		expanded := clierr.ExpandRemedies(s, "")
		assert.NotRegexp(t, loginRE, expanded, name)
		if !removes(name) {
			assert.NotRegexp(t, removeRE, expanded, name)
		}
	}
}

// signInLeftoverFates is one token fate for each way the exec plugin's
// sign-in can leave a token behind: an ext token and a v3 token with a
// name, a marked token of ext, v3 and unknown kind with none, and an
// unmarked token found only through the SAML handoff.
func signInLeftoverFates() map[string]clierr.TokenFate {
	return map[string]clierr.TokenFate{
		"ext with a name":      {Ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc"}, ExpiresAt: "2026-10-12T12:00:00Z"},
		"ext, no expiry":       {Ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "token-abc"}},
		"v3 with a name":       {Ref: &clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "token-v3"}, ExpiresAt: "2026-10-12T12:00:00Z"},
		"marked ext":           {Marked: true, Kind: clierr.TokenKindExt},
		"marked v3":            {Marked: true, Kind: clierr.TokenKindV3},
		"marked, unknown":      {Marked: true},
		"the SAML handoff":     {Search: &clierr.UnmarkedTokenSearch{AuthProvider: "keycloakSAML", HandoffAt: "2026-10-12T10:00:00Z", PollEndedAt: "2026-10-12T10:15:00Z"}},
		"the SAML with an end": {Search: &clierr.UnmarkedTokenSearch{AuthProvider: "keycloakSAML", HandoffAt: "2026-10-12T10:00:00Z", PollEndedAt: "2026-10-12T10:15:00Z", RequestExpiresAt: "2026-10-12T10:30:00Z"}},
	}
}
