package token

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// refusedNames are values no printed command can hold exactly: a single
// quote, a right single quotation mark, a zero width space and two
// backslashes in a row.
var refusedNames = []struct{ name, value string }{
	{"a single quote", "a'b"},
	{"a right single quotation mark", "a\u2019b"},
	{"a zero width space", "a\u200bb"},
	{"two backslashes", `a\\b`},
}

// assertDeleteLeftOut asserts text, once the remedy marker is expanded, has no
// delete command of the token kind, names the command that lists the tokens
// of that kind, and holds the note about the left-out command once.
func assertDeleteLeftOut(t *testing.T, text string, kind clierr.TokenKind) {
	t.Helper()
	text = clierr.ExpandRemedies(text, "")
	deleteWords, list := "delete token", "rancher get tokens"
	if kind == TokenKindV3 {
		deleteWords, list = "delete management-token", "rancher get management-tokens"
	}
	assert.NotContains(t, text, deleteWords)
	assert.Contains(t, text, list)
	assert.Equal(t, 1, strings.Count(text, clierr.LeftOutNote), text)
}

// TestRevokeTextsLeaveOutTheDeleteOfATokenNameThatCannotBePrinted asserts that
// every text of this package that names the delete of a token (the warning of
// a superseded token that was kept, the recovery of a login failure and its
// hint, the sign-in leftover, the clause of a refused owner check and the
// warning of a delete plan) gives no delete command for a name that cannot be
// printed exactly, gives the command that lists the tokens of its kind, and
// carries the note about the left-out command once.
func TestRevokeTextsLeaveOutTheDeleteOfATokenNameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	const url = "https://r.example.com"
	for _, tc := range refusedNames {
		for _, kind := range []clierr.TokenKind{TokenKindExt, TokenKindV3} {
			t.Run(tc.name+", "+string(kind), func(t *testing.T) {
				t.Parallel()
				ref := TokenRef{Kind: kind, Name: tc.value}
				fate := clierr.TokenFate{Ref: &ref}
				assertDeleteLeftOut(t, keptWarning(config.SupersededToken{Ref: ref, UserID: "u-abc12"}, url, "the server refused"), kind)
				for _, persisted := range []bool{true, false} {
					text, hint := ResidualRecovery(fate, "prod", "m", persisted)
					assertDeleteLeftOut(t, text, kind)
					if kind == TokenKindExt && persisted {
						assert.Equal(t, "rancher get tokens", clierr.ExpandRemedies(hint.Field(), ""), "the hint of a left-out revoke is the list command")
					}
				}
				assertDeleteLeftOut(t, signInLeftover(fate, "u-abc12", "m"), kind)
				assertDeleteLeftOut(t, signInLeftover(fate, "", "m"), kind)
				assertDeleteLeftOut(t, revokedClause("the token", &ref, RevokeOutcome{Result: ResultFailed}), kind)

				cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: url}}}
				d := &Deletion{cfg: cfg, view: cfg, removed: map[string]bool{}}
				it := &revocationItem{source: sourceCache, ref: &ref, server: config.CanonicalServerIdentity(url), owner: "u-abc12", entry: "prod", reason: reasonFailed}
				assertDeleteLeftOut(t, d.manualWarning(it), kind)
				commands, _ := d.recovery(it)
				require.Len(t, commands, 1, "a left-out recovery is never an empty list")
				list := "rancher get tokens"
				if kind == TokenKindV3 {
					list = "rancher get management-tokens"
				}
				assert.Equal(t, list, clierr.ExpandRemedies(commands[0], ""))
			})
		}
	}
}

// TestFailureRecoveryHintOfALeftOutRevokeIsTheListCommand asserts the hint of
// a login failure that left a token with a name that cannot be printed is the
// command that lists the tokens, and that the message carries the note once.
func TestFailureRecoveryHintOfALeftOutRevokeIsTheListCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ref := &TokenRef{Kind: TokenKindExt, Name: tc.value}
			cleanup := RevokeOutcome{Ref: ref, Result: ResultFailed, Error: "500"}
			cred := LoginCredential{Provenance: ProvenanceDerived, Ref: ref, Marker: "m"}
			states := LoginStates{Commit: &config.CommitOutcome{Backend: clierr.BackendKeyring, State: config.CommitCommitted}, CredentialCleanup: &cleanup}
			err := FailLogin(clierr.New(clierr.CodeGeneric, "the login failed"), clierr.StageCommit, states, cred, "prod")
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			assert.Equal(t, "rancher get tokens", clierr.ExpandRemedies(ce.Hint, ""))
			assert.Equal(t, 1, strings.Count(ce.Message, clierr.LeftOutNote), ce.Message)
		})
	}
}

// TestRegenerateStepsAndTheNamedEntryRouteAreLeftOutForAnEntryThatCannotBePrinted
// asserts the steps that write the kubeconfig again are left out whole for an
// entry with a quote in its name, and that the refusals that offer them say
// so once and give no kubeconfig command.
func TestRegenerateStepsAndTheNamedEntryRouteAreLeftOutForAnEntryThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	const url, other = "https://r.example.com", "https://other.example.com"
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			steps := RegenerateSteps(clierr.NewCommand, tc.value, "c-1")
			assert.False(t, steps.OK())
			assert.Nil(t, steps.Lines())
			text := clierr.Text("regenerate the kubeconfig", clierr.Lead(": "), steps, RegenerateNote)
			assert.NotContains(t, text, "kubeconfig --cluster")
			assert.Equal(t, 1, strings.Count(text, clierr.LeftOutNote), text)

			route, ok := namedEntryRoute(tc.value, "c-1", "prod")
			require.True(t, ok)
			assert.False(t, route.OK())

			elsewhere := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{tc.value: {URL: other}, "prod": {URL: other}}}
			registeredHere := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{tc.value: {URL: other}, "prod": {URL: url}}}
			for name, cfg := range map[string]*config.Config{"no entry at the server": elsewhere, "an entry at the server": registeredHere} {
				ce := entryOnOtherServerError(cfg, tc.value, url, "c-1")
				message := clierr.ExpandRemedies(ce.Message, "")
				assert.NotContains(t, message, "rancher kubeconfig --cluster 'c-1'; then", name)
				assert.NotContains(t, message, "if it has no login", name)
				assert.Equal(t, 1, strings.Count(message, clierr.LeftOutNote), name+": "+message)
			}
		})
	}
}

// TestTheKubeconfigStepsOfAnAmbiguousChoiceAreLeftOutForAClusterThatCannotBePrinted
// asserts a cluster id that cannot be printed leaves out the --name clause
// and the regenerate steps, with the note once, and that a cluster id that
// needs quoting is quoted in the --name clause and a plain one is not.
func TestTheKubeconfigStepsOfAnAmbiguousChoiceAreLeftOutForAClusterThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{CurrentServer: "a", Servers: map[string]config.Server{"a": {URL: "https://r.example.com"}, "b": {URL: "https://r.example.com"}}}
	hits := []storedCredential{{name: "a", entry: cfg.Servers["a"]}, {name: "b", entry: cfg.Servers["b"]}}
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, ignoreCurrent := range []bool{false, true} {
				ce := entryNotNamedError(cfg, hits, tc.value, ignoreCurrent)
				message := clierr.ExpandRemedies(ce.Message, "")
				assert.NotContains(t, message, "--name <entry>-")
				assert.NotContains(t, message, "kubeconfig --cluster")
				assert.Equal(t, 1, strings.Count(message, clierr.LeftOutNote), message)
			}
		})
	}
	plain := clierr.ExpandRemedies(entryNotNamedError(cfg, hits, "c-1", false).Message, "")
	assert.Contains(t, plain, "give the exec entry --name <entry>-c-1; or regenerate the kubeconfig from the entry you want with: rancher config use-server <entry>; then rancher kubeconfig --cluster 'c-1'")
	spaced := clierr.ExpandRemedies(entryNotNamedError(cfg, hits, "c 1", false).Message, "")
	assert.Contains(t, spaced, "give the exec entry --name <entry>-'c 1'; or regenerate")
}

// TestTheEntriesAnswerRefusalLeavesOutTheLoginOfAnEntryThatCannotBePrinted
// asserts the refusal of several plugin login entries gives no login
// command, and no hint, for an identified entry whose name cannot be
// printed, and says so once.
func TestTheEntriesAnswerRefusalLeavesOutTheLoginOfAnEntryThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	const url = "https://r.example.com"
	cfg := &config.Config{Servers: map[string]config.Server{
		"exec-login-r.example.com":   {URL: url, Provenance: clierr.ProvenanceExecLogin},
		"exec-login-r.example.com-2": {URL: url, Provenance: clierr.ProvenanceExecLogin},
	}}
	names := []string{"exec-login-r.example.com", "exec-login-r.example.com-2"}
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ce := pluginEntriesError(url, names, cfg, tc.value, userCheck{userID: "u-abc12", clusterID: "c-1", serverID: url})
			message := clierr.ExpandRemedies(ce.Message, "")
			if config.ValidateEntryName(tc.value) != nil {
				return
			}
			assert.NotContains(t, message, "auth login 'a")
			assert.Empty(t, ce.Hint)
			assert.Equal(t, 1, strings.Count(message, clierr.LeftOutNote), message)
		})
	}
}

// TestTheLoginOfAnEntryThatCannotBePrintedIsLeftOutOfTheOwnerRelogin asserts
// the relogin text of an owner mismatch gives no auth login command and no
// hint for an entry name that cannot be printed, and says so once.
func TestTheLoginOfAnEntryThatCannotBePrintedIsLeftOutOfTheOwnerRelogin(t *testing.T) {
	t.Parallel()
	u := userCheck{userID: "u-abc12", clusterID: "c-1", serverID: "https://r.example.com"}
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			text, hint := u.relogin(&config.Config{}, tc.value, false)
			assert.NotContains(t, clierr.ExpandRemedies(text, ""), "auth login 'a")
			assert.Empty(t, hint.Field())
			assert.False(t, hint.OK())
			assert.Equal(t, 1, strings.Count(text, clierr.LeftOutNote), text)

			ce := u.entryOwnerRefused(&config.Config{}, tc.value, "u-other", false, true)
			assert.Empty(t, ce.Hint)
			assert.Equal(t, 1, strings.Count(ce.Message, clierr.LeftOutNote), ce.Message)
		})
	}
}

// TestTheServerOfALoginThatCannotBePrintedLeavesOutTheLoginSteps asserts a
// login for a server URL that cannot be printed is left out whole, steps
// after it included, with the note once, and gives no hint.
func TestTheServerOfALoginThatCannotBePrintedLeavesOutTheLoginSteps(t *testing.T) {
	t.Parallel()
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			login := NoTerminalLogin(&config.Config{}, "", "c-1", "https://"+tc.value, "", config.CASource{}, false)
			assert.False(t, login.Login().OK())
			assert.Equal(t, clierr.LeftOutNote, login.Text())
			text := login.Instruction("to log in, pipe an API token")
			assert.NotContains(t, text, "auth login")
			assert.Equal(t, 1, strings.Count(text, clierr.LeftOutNote), text)
			assert.Empty(t, login.Hint().Field())
		})
	}
	unusable := userCheck{clusterID: "c-1"}.loginOfNewEntry(&config.Config{})
	assert.NotContains(t, unusable, "auth login", "no server URL to name leaves the command out")
	assert.Equal(t, 1, strings.Count(unusable, clierr.LeftOutNote), unusable)
}

// TestTheAuthProviderFragmentOfTheNoTerminalRefusalIsDroppedForANameThatCannotBePrinted
// asserts the clause that asks for --auth-provider with the one auth config
// that signs in without a terminal is kept for a plain name, quotes a name
// that needs it, and is dropped, with the note once, for a name that cannot be
// printed.
func TestTheAuthProviderFragmentOfTheNoTerminalRefusalIsDroppedForANameThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	const url = "https://r.example.com"
	call := noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: url}
	refuse := func(t *testing.T, id string) string {
		t.Helper()
		cause := &providerChoiceNeedsTerminal{text: "choose", configs: []string{"local", id}, noTerminal: []clierr.Candidate{{ID: id, Name: id}}}
		err := tokenCmd{dir: t.TempDir()}.withNoTerminalRemedies(cause, call)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "got %v", err)
		return clierr.ExpandRemedies(ce.Message, "")
	}
	assert.Contains(t, refuse(t, "corp-saml"), "or add --auth-provider corp-saml to the command (in a kubeconfig, to the exec entry's args) to sign in through corp-saml in a browser")
	assert.Contains(t, refuse(t, "corp saml"), "or add --auth-provider 'corp saml' to the command")
	for _, tc := range refusedNames {
		message := refuse(t, tc.value)
		assert.NotContains(t, message, "--auth-provider", tc.name)
		assert.NotContains(t, message, "or add", tc.name)
		assert.Equal(t, 1, strings.Count(message, clierr.LeftOutNote), tc.name+": "+message)
	}
}

// TestTheRefreshCommandOfARejectedStoredLoginNamesTheConfigDirectoryOfTheRun
// asserts the notice of a stored login the server rejected relays its
// command with the remedy marker, so a run against a config directory prints
// the command with --config naming that directory, and that an entry name
// that cannot be printed leaves the command out with the note.
func TestTheRefreshCommandOfARejectedStoredLoginNamesTheConfigDirectoryOfTheRun(t *testing.T) {
	t.Parallel()
	rejected := func(name string) string {
		var written bytes.Buffer
		p := newEventPrompter(ui.New(&written, nil, true, false))
		err := tokenCmd{}.classifyRejection(errCredentialUnauthorized, storedCredential{name: name}, "", true, p)
		require.ErrorIs(t, err, errStoredCredentialRejected)
		return noticeWith(t, p, "was rejected")
	}
	expanded := runtime.IO{RemedyDir: "/d"}.ExpandText(rejected("prod"))
	assert.Contains(t, expanded, "; refresh it with: rancher --config '/d' auth login 'prod'")
	for _, tc := range refusedNames {
		notice := runtime.IO{RemedyDir: "/d"}.ExpandText(rejected(tc.value))
		assert.NotContains(t, notice, "auth login", tc.name)
		assert.Equal(t, 1, strings.Count(notice, clierr.LeftOutNote), tc.name+": "+notice)
	}
}

// TestTheDeleteTokenRefusalDropsTheClauseOfAnArgumentThatCannotBePrinted
// asserts the usage error of a name together with --all drops the clause that
// deletes that one name when the name cannot be printed, keeps the command
// that deletes every credential and says so once.
func TestTheDeleteTokenRefusalDropsTheClauseOfAnArgumentThatCannotBePrinted(t *testing.T) {
	t.Parallel()
	for _, tc := range refusedNames {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := deleteTokenCmd{dir: t.TempDir()}.Execute(t.Context(), tin{a: []string{tc.value}, b: map[string]bool{"all": true}}, ui.Discard())
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok)
			message := clierr.ExpandRemedies(ce.Message, "")
			assert.NotContains(t, message, "to delete only")
			assert.Contains(t, message, "to delete every cached credential, run: rancher auth delete-token --all")
			assert.Equal(t, 1, strings.Count(message, clierr.LeftOutNote), message)
		})
	}
}

// TestTheRerunOfALocalDeleteIsLeftOutNotPrintedEmpty asserts a rerun command
// with a value that cannot be printed leaves out the clause that names it in
// the failure text and in the warning of a delete plan, with the note, and
// leaves the recovery list empty rather than naming an empty command.
func TestTheRerunOfALocalDeleteIsLeftOutNotPrintedEmpty(t *testing.T) {
	t.Parallel()
	rerun := clierr.NewCommand("auth logout").Arg("a'b")
	text := deleteFailureText(`"a'b"`, errors.New("the lock timed out"), rerun)
	assert.NotContains(t, text, "run:")
	assert.Equal(t, 1, strings.Count(text, clierr.LeftOutNote), text)

	const url = "https://r.example.com"
	cfg := &config.Config{Servers: map[string]config.Server{"prod": {URL: url}}}
	d := &Deletion{cfg: cfg, view: cfg, req: LocalDelete{Rerun: rerun}}
	ref := &TokenRef{Kind: TokenKindExt, Name: "token-abc"}
	it := &revocationItem{source: sourceLogin, ref: ref, server: config.CanonicalServerIdentity(url), entry: "prod", reason: reasonNotDeleted}
	warning := d.manualWarning(it)
	assert.NotContains(t, warning, "run:")
	assert.Equal(t, 1, strings.Count(warning, clierr.LeftOutNote), warning)
	commands, _ := d.recovery(it)
	assert.Empty(t, commands)
}
