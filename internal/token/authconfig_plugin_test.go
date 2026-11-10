package token

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
)

// TestASeveralMatchWithoutATerminalNamesTheConfigsToThePlugin asserts a
// --auth-provider value that matches several auth configs, with no terminal,
// gives the exec plugin's remedies naming only the matching configs: the
// two Azure AD configs, which sign in with no terminal, are
// AMBIGUOUS_SELECTION with --auth-provider and the two names; the two LDAP
// configs, which need one, are the AUTH refusal.
func TestASeveralMatchWithoutATerminalNamesTheConfigsToThePlugin(t *testing.T) {
	isolateSignIn(t)
	for _, tc := range []struct {
		name       string
		entries    [][2]string
		want       string
		code       string
		candidates []string
		head       string
		choice     string
	}{
		{
			name:       "two Azure AD configs",
			entries:    [][2]string{{"local", "localProvider"}, {"corp-azure", "azureADProvider"}, {"eu-azure", "azureADProvider"}, {"corp-ldap", "openLdapProvider"}},
			want:       "azureADProvider",
			code:       clierr.CodeAmbiguousSelection,
			candidates: []string{"corp-azure", "eu-azure"},
			head:       "this server has several auth configs and this call has no terminal to choose one; to log in without a terminal, pipe an API token to: ",
			choice:     "; or add --auth-provider with one of corp-azure, eu-azure to the command (in a kubeconfig, to the exec entry's args) and sign in through it in a browser",
		},
		{
			name:    "two LDAP configs",
			entries: [][2]string{{"local", "localProvider"}, {"corp-ldap", "openLdapProvider"}, {"eu-ldap", "openLdapProvider"}, {"corp-azure", "azureADProvider"}},
			want:    "openLdapProvider",
			code:    clierr.CodeAuth,
			head:    "interactive login required for one of the auth configs corp-ldap, eu-ldap; this call has no terminal to answer it; to log in without a terminal, pipe an API token to: ",
		},
	} {
		for _, lane := range choiceLanes {
			t.Run(tc.name+", "+lane, func(t *testing.T) {
				srv := configsServer(t, tc.entries...)
				err := runProviderChoice(t, srv, lane, noTerminal(), map[string]string{"auth-provider": tc.want}, false)
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "got %v", err)
				assert.Equal(t, tc.code, ce.Code)
				assert.Equal(t, tc.candidates, candidateIDsOrNil(ce.Candidates))
				if tc.code == clierr.CodeAmbiguousSelection {
					assert.True(t, strings.HasPrefix(strings.Replace(ce.Message, "<name> stands for a name of your choice; ", "", 1), tc.head), "message %q", ce.Message)
					require.ErrorContains(t, err, tc.choice)
					return
				}
				assert.True(t, strings.HasPrefix(strings.Replace(ce.Message, "<name> stands for a name of your choice; ", "", 1), tc.head), "message %q", ce.Message)
				assert.NotContains(t, ce.Message, "--auth-provider")
			})
		}
	}
}

// TestASignInThroughAnotherConfigIsRefusedForAUserID asserts the owner check
// holds across auth configs of one type: a call naming the default config's
// user with --user-id and signing in through corp-ldap is a sign-in as
// another user and is refused, and the same call through the default config
// succeeds.
func TestASignInThroughAnotherConfigIsRefusedForAUserID(t *testing.T) {
	isolateSignIn(t)
	call := func(t *testing.T, authProvider string) error {
		t.Helper()
		x := newPluginFixture(t, false, twoLDAPConfigs())
		_, err := x.command(x.store).Execute(t.Context(), tin{s: map[string]string{
			"server": x.url, "cluster": "c-test", "name": "n", "user-id": "user-default", "auth-provider": authProvider,
		}}, terminalLines("alice", "pw"))
		return err
	}
	err := call(t, "corp-ldap")
	require.ErrorContains(t, err, "the sign-in was made as Rancher user 'user-corp', and this exec entry names user 'user-default' with --user-id")
	require.NoError(t, call(t, "openldap"))
}
