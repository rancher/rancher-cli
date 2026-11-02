package token

import (
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
)

// providersServer is a Rancher that advertises one auth config of each given
// provider type, named after its provider name as every config is up to
// v2.15, in that order, and answers nothing else.
func providersServer(t *testing.T, types ...string) *httptest.Server {
	t.Helper()
	var items []string
	for _, typ := range types {
		items = append(items, `{"id":"`+providerName(typ)+`","type":"`+typ+`"}`)
	}
	body := `{"data":[` + strings.Join(items, ",") + `]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// noInputWithTerminal is the prompter of a call under --no-input.
func noInputWithTerminal() *ui.Prompter { return ui.New(io.Discard, strings.NewReader(""), true, true) }

const unfinishedChangeID = "0123456789abcdef0123456789abcdef"

// choiceLanes are the three ways a call reaches the sign-in: the exec
// plugin with token reuse on (its plugin-created login), the same command
// with token reuse off (the interactive login of a call that keeps no
// login), and the legacy shim, which names no cluster here.
var choiceLanes = []string{"plugin login", "no plugin login", "legacy shim"}

// runProviderChoice runs one call of the given lane against srv with p and
// returns its error. unfinished leaves a credential store change
// unfinished in the directory first.
func runProviderChoice(t *testing.T, srv *httptest.Server, lane string, p runtime.Prompter, extra map[string]string, unfinished bool) error {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "rancher")
	cfg := &config.Config{CredentialStore: "file", TokenReuse: lane == "plugin login"}
	if unfinished {
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(config.TransitionPath(dir), []byte("version: 1\nid: "+unfinishedChangeID+"\nsteps:\n- kind: pin\n  target: file\n"), 0o600))
		cfg.CredentialTransitionID = unfinishedChangeID
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	flags := map[string]string{"server": srv.URL}
	var cmd runtime.ReadCommand
	switch lane {
	case "plugin login", "no plugin login":
		flags["cluster"], flags["name"] = "c-1", "prod-c-1"
		cmd = NewTokenCommand(dir, config.NewFileStore(dir), anchorFor(dir)).(runtime.ReadCommand)
	default:
		flags["user"] = "alice"
		cmd = NewLegacyTokenCommand(dir, config.NewFileStore(dir), anchorFor(dir)).(runtime.ReadCommand)
	}
	maps.Copy(flags, extra)
	_, err := cmd.Execute(t.Context(), tin{s: flags}, p)
	require.Error(t, err)
	return err
}

// TestAProviderChoiceWithoutATerminalNamesTheRemedies asserts the exec
// plugin's refusal to pick one of several auth providers with no terminal
// to ask on: on every lane it is a fresh error with the no-terminal
// remedies, no login-failure details and no "run the kubectl command
// again" recovery, whose candidates are only the providers that sign in
// with no terminal, and is a plain AUTH refusal with none when no provider
// can.
func TestAProviderChoiceWithoutATerminalNamesTheRemedies(t *testing.T) {
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCredentialStore, "")
	t.Setenv(execInfoEnv, "")
	keyring.MockInit()

	for _, tc := range []struct {
		name      string
		providers []string
		prompter  func() runtime.Prompter
		extra     map[string]string
		unfinish  bool
		code      string
		// candidates are the auth config names the error offers.
		candidates []string
		// choice is the --auth-provider sentence; "" when none is named.
		choice string
		// head is the start of the message.
		head string
		// tail ends the message.
		tail string
	}{
		{
			name:      "only basic providers",
			providers: []string{"localProvider", "openLdapProvider"},
			code:      clierr.CodeAuth,
			head:      "interactive login required for one of the auth configs local, openldap; this call has no terminal to answer it; to log in without one, pipe an API token to: ",
		},
		{
			name:       "one provider signs in without a terminal",
			providers:  []string{"localProvider", "oktaProvider"},
			code:       clierr.CodeAmbiguousSelection,
			candidates: []string{"okta"},
			head:       "this server has several auth configs and this call has no terminal to choose one; to log in without one, pipe an API token to: ",
			choice:     "; or add --auth-provider okta to the command (in a kubeconfig, to the exec entry's args) to sign in through okta in a browser",
		},
		{
			name:       "several providers sign in without a terminal",
			providers:  []string{"localProvider", "oktaProvider", "azureADProvider"},
			code:       clierr.CodeAmbiguousSelection,
			candidates: []string{"okta", "azuread"},
			head:       "this server has several auth configs and this call has no terminal to choose one; to log in without one, pipe an API token to: ",
			choice:     "; or add --auth-provider with one of okta, azuread to the command (in a kubeconfig, to the exec entry's args) and sign in through it in a browser",
		},
		{
			name:      "no-input leaves no provider that signs in",
			providers: []string{"localProvider", "oktaProvider"},
			prompter:  func() runtime.Prompter { return noInputWithTerminal() },
			code:      clierr.CodeAuth,
			head:      "interactive login required for one of the auth configs local, okta; this call has no terminal to answer it; to log in without one, pipe an API token to: ",
		},
		{
			name:       "a call for a user",
			providers:  []string{"localProvider", "oktaProvider"},
			extra:      map[string]string{"user-id": "u-abc12"},
			code:       clierr.CodeAmbiguousSelection,
			candidates: []string{"okta"},
			head:       "this server has several auth configs and this call has no terminal to choose one; to log in without one, pipe an API token of Rancher user " + clierr.ShellQuote("u-abc12") + " to: ",
			choice:     "; or add --auth-provider okta to the command (in a kubeconfig, to the exec entry's args) to sign in through okta in a browser",
		},
		{
			name:       "an unsupported provider advertised",
			providers:  []string{"localProvider", "oktaProvider", "githubProvider"},
			code:       clierr.CodeAmbiguousSelection,
			candidates: []string{"okta"},
			head:       "this server has several auth configs and this call has no terminal to choose one; to log in without one, pipe an API token to: ",
			choice:     "; or add --auth-provider okta to the command (in a kubeconfig, to the exec entry's args) to sign in through okta in a browser",
			tail:       "; " + unsupportedNote([]string{"githubProvider"}),
		},
		{
			name:      "an unsupported provider advertised, none that signs in",
			providers: []string{"localProvider", "openLdapProvider", "githubProvider"},
			code:      clierr.CodeAuth,
			head:      "interactive login required for one of the auth configs local, openldap; this call has no terminal to answer it; to log in without one, pipe an API token to: ",
			tail:      "; " + unsupportedNote([]string{"githubProvider"}),
		},
		{
			name:       "a store change left unfinished",
			providers:  []string{"localProvider", "oktaProvider"},
			unfinish:   true,
			code:       clierr.CodeAmbiguousSelection,
			candidates: []string{"okta"},
			head:       "this server has several auth configs and this call has no terminal to choose one; to log in without one, pipe an API token to: ",
			choice:     "; or add --auth-provider okta to the command (in a kubeconfig, to the exec entry's args) to sign in through okta in a browser",
			tail:       "; a credential store change is unfinished; see it with: " + clierr.Rancher + " config status",
		},
	} {
		for _, lane := range choiceLanes {
			if lane == "legacy shim" && tc.extra["user-id"] != "" {
				// The shim has no --user-id.
				continue
			}
			t.Run(tc.name+", "+lane, func(t *testing.T) {
				srv := providersServer(t, tc.providers...)
				var p runtime.Prompter = noTerminal()
				if tc.prompter != nil {
					p = tc.prompter()
				}
				err := runProviderChoice(t, srv, lane, p, tc.extra, tc.unfinish)

				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "got %v", err)
				assert.Equal(t, tc.code, ce.Code)
				_, ok = errors.AsType[*providerChoiceNeedsTerminal](err)
				assert.True(t, ok, "the cause stays on the error the command returns")
				assert.Nil(t, ce.Details, "no login-failure details: nothing was signed in")
				assert.NotContains(t, ce.Message, "run the kubectl command again")
				assert.Equal(t, tc.candidates, candidateIDsOrNil(ce.Candidates))
				require.True(t, strings.HasPrefix(strings.Replace(ce.Message, "<name> stands for a name of your choice; ", "", 1), tc.head), "message %q", ce.Message)
				require.ErrorContains(t, err, clierr.Rancher+" auth login ")
				require.ErrorContains(t, err, "--token-stdin")
				if tc.choice == "" {
					assert.NotContains(t, ce.Message, "--auth-provider")
				} else {
					require.ErrorContains(t, err, tc.choice)
				}
				if tc.tail != "" {
					assert.True(t, strings.HasSuffix(ce.Message, tc.tail), "message %q", ce.Message)
				}
				if !strings.Contains(tc.tail, "unfinished") {
					assert.NotContains(t, ce.Message, "config status")
				}
			})
		}
	}
}

func candidateIDsOrNil(cs []clierr.Candidate) []string {
	if len(cs) == 0 {
		return nil
	}
	return candidateIDs(cs)
}

// TestAProviderChoiceRemediesMatchTheLane asserts the lane's own sentences
// follow the provider choice: the plugin login keeps a login credential,
// so a terminal sign-in is kept; the legacy shim's login is no hint.
func TestAProviderChoiceRemediesMatchTheLane(t *testing.T) {
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCredentialStore, "")
	t.Setenv(execInfoEnv, "")
	keyring.MockInit()
	srv := providersServer(t, "localProvider", "oktaProvider")
	url := config.CanonicalServerIdentity(srv.URL)

	err := runProviderChoice(t, srv, "plugin login", noTerminal(), nil, false)
	require.ErrorContains(t, err, "or run kubectl once in a terminal and sign in")
	assert.NotContains(t, err.Error(), "until the cached token expires")
	ce, _ := errors.AsType[*clierr.Error](err)
	assert.Equal(t, clierr.Rancher+" auth login 'prod' --url "+clierr.ShellQuote(url)+" --token-stdin", ce.Hint)

	err = runProviderChoice(t, srv, "no plugin login", noTerminal(), nil, false)
	require.ErrorContains(t, err, "or run kubectl once in a terminal and sign in, which lasts until the cached token expires")

	err = runProviderChoice(t, srv, "legacy shim", noTerminal(), nil, false)
	require.ErrorContains(t, err, "<name> stands for a name of your choice; to log in without one, pipe an API token to: "+clierr.Rancher+" auth login <name> --url "+clierr.ShellQuote(url)+" --token-stdin")
	ce, _ = errors.AsType[*clierr.Error](err)
	assert.Empty(t, ce.Hint, "the shim's slot is no name auth login accepts")
}

// TestSelectAuthProviderRecordsWhoSignsInWithoutATerminal asserts the
// refusal between several providers carries the cause the exec plugin
// builds its remedies from when nobody can answer the menu, with the
// providers that need no terminal - SAML and OAuth, and only without
// --no-input - and carries none when someone could answer it and gave an
// invalid pick.
func TestSelectAuthProviderRecordsWhoSignsInWithoutATerminal(t *testing.T) {
	t.Parallel()
	azure := &AzureADProvider{}
	azure.Type = "azureADProvider"
	azure.ID = "azuread"
	ps := []typedProvider{
		&AuthProvider{Type: "localProvider", ID: "local"}, &AuthProvider{Type: "oktaProvider", ID: "okta"}, azure, &AuthProvider{Type: "openLdapProvider", ID: "openldap"},
	}

	_, err := selectAuthProvider(t.Context(), ps, []string{"githubProvider"}, "", false, noTerminal())
	cause, ok := errors.AsType[*providerChoiceNeedsTerminal](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, []string{"local", "okta", "azuread", "openldap"}, cause.configs)
	assert.Equal(t, []clierr.Candidate{{ID: "okta", Name: "okta"}, {ID: "azuread", Name: "azuread"}}, cause.noTerminal)
	assert.Equal(t, unsupportedNote([]string{"githubProvider"}), cause.note)

	_, err = selectAuthProvider(t.Context(), ps, nil, "", false, noInputWithTerminal())
	cause, ok = errors.AsType[*providerChoiceNeedsTerminal](err)
	require.True(t, ok, "got %v", err)
	assert.Empty(t, cause.noTerminal, "--no-input refuses the SAML and OAuth flows too")

	var errBuf strings.Builder
	_, err = selectAuthProvider(t.Context(), ps, nil, "", false, ui.New(&errBuf, newLineReader("not a number"), false, true))
	require.Error(t, err)
	_, ok = errors.AsType[*providerChoiceNeedsTerminal](err)
	assert.False(t, ok, "someone was there to answer: an invalid pick is today's plain error")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
	assert.Equal(t, "this server has several auth configs: local, okta, azuread, openldap; pass one of them with --auth-provider", ce.Message)
	assert.Len(t, ce.Candidates, 4)
}

// TestAnInvalidProviderPickWithATerminalIsThePlainError asserts the
// refusal of an invalid menu pick, where someone could answer, is the
// plain AMBIGUOUS_SELECTION with every supported provider as a candidate:
// no piped-token login, no --auth-provider sentence and no disclosure, and
// no cause the no-terminal remedies build on. In the plugin-created login
// it arrives as a failed acquisition with its login-failure details and
// recovery; with token reuse off it carries no details.
func TestAnInvalidProviderPickWithATerminalIsThePlainError(t *testing.T) {
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCredentialStore, "")
	t.Setenv(execInfoEnv, "")
	keyring.MockInit()
	for _, lane := range []string{"plugin login", "no plugin login"} {
		t.Run(lane, func(t *testing.T) {
			srv := providersServer(t, "localProvider", "oktaProvider")
			p := terminalLines("not a number")
			err := runProviderChoice(t, srv, lane, p, nil, false)

			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
			assert.Equal(t, []string{"local", "okta"}, candidateIDsOrNil(ce.Candidates))
			assert.True(t, strings.HasPrefix(ce.Message, "this server has several auth configs: local, okta; pass one of them with --auth-provider"), "message %q", ce.Message)
			assert.NotContains(t, ce.Message, "pipe an API token")
			assert.NotContains(t, ce.Message, "or add --auth-provider")
			_, ok = errors.AsType[*providerChoiceNeedsTerminal](err)
			assert.False(t, ok, "someone could answer the menu")
			assert.Zero(t, p.count(plaintextDisclosureMark), "nothing was signed in: no disclosure; events %q", p.events)
			if lane == "plugin login" {
				details, ok := ce.Details.(clierr.LoginFailureDetails)
				require.True(t, ok, "details %T", ce.Details)
				assert.Equal(t, clierr.StageAuthentication, details.Stage)
				assert.Contains(t, details.Recovery, "run the kubectl command again")
				return
			}
			assert.Nil(t, ce.Details)
		})
	}
}
