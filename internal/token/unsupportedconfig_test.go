package token

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// unsupportedRow is an auth config of a provider type this CLI cannot sign
// in with: its id, which differs from the provider name so the three values
// --auth-provider can take differ, its provider type and its provider name.
type unsupportedRow struct{ id, typ, name string }

// unsupportedRows are the seven provider types the server lists that this
// CLI cannot use, and a type no Rancher lists today.
var unsupportedRows = []unsupportedRow{
	{"corp-gh", "githubProvider", "github"},
	{"corp-ghapp", "githubAppProvider", "githubapp"},
	{"corp-google", "googleOAuthProvider", "googleoauth"},
	{"corp-oidc", "oidcProvider", "oidc"},
	{"corp-kc", "keyCloakOIDCProvider", "keycloakoidc"},
	{"corp-generic", "genericOIDCProvider", "genericoidc"},
	{"corp-cognito", "cognitoProvider", "cognito"},
	{"corp-foo", "fooProvider", "foo"},
}

// values are what --auth-provider can take to name the row's auth config.
func (u unsupportedRow) values() map[string]string {
	return map[string]string{"id": u.id, "type": u.typ, "provider name": u.name}
}

// unsupportedNoteOfRows is the note every refusal ends with: all the types
// of unsupportedRows, sorted.
func unsupportedNoteOfRows() string {
	types := make([]string, 0, len(unsupportedRows))
	for _, u := range unsupportedRows {
		types = append(types, u.typ)
	}
	slices.Sort(types)
	return "advertised by the server but not supported by this CLI: " + strings.Join(types, ", ") +
		" (server change requested: rancher/rancher#57034); " +
		"to sign in as a user of one of them, create an API token in the Rancher UI and log in with it using auth login --token-stdin"
}

// unsupportedRefusal is the message of the refusal of value for row.
func unsupportedRefusal(value string, u unsupportedRow) string {
	return "--auth-provider " + value + " names an auth config this CLI cannot sign in with: " + u.id + " (" + u.typ + "); " + unsupportedNoteOfRows()
}

// providerListServer answers the server's list of auth configs with the
// entries of unsupportedRows, and the local auth config when withLocal.
func providerListServer(t *testing.T, withLocal bool) *httptest.Server {
	t.Helper()
	var entries []string
	if withLocal {
		entries = append(entries, `{"id":"local","type":"localProvider"}`)
	}
	for _, u := range unsupportedRows {
		entries = append(entries, `{"id":"`+u.id+`","type":"`+u.typ+`"}`)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[` + strings.Join(entries, ",") + `]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEveryUnsupportedProviderTypeIsRefusedByIDTypeAndName asserts the rule
// for every provider type the server lists that this CLI cannot sign in
// with, the seven Rancher lists and one it does not: --auth-provider given
// as the auth config's id, its provider type or its provider name is
// CAPABILITY_UNSUPPORTED, exit 11, naming the auth config and its type, with
// the supported auth configs as candidates (none when there are none).
// Mutations: refuse only by id (the type and provider name rows fail); read
// an unknown type as a miss (the fooProvider rows fail).
func TestEveryUnsupportedProviderTypeIsRefusedByIDTypeAndName(t *testing.T) {
	t.Parallel()
	for _, withLocal := range []bool{true, false} {
		srv := providerListServer(t, withLocal)
		for _, u := range unsupportedRows {
			for form, value := range u.values() {
				name := u.typ + " by " + form
				if !withLocal {
					name += ", no supported auth config"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					_, err := loginAndGenerateCred(t.Context(), srv.Client(), &loginInput{caFlag: "--cacert", server: srv.URL, authProvider: value}, noTerminal())
					ce, ok := errors.AsType[*clierr.Error](err)
					require.True(t, ok, "got %v", err)
					assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
					assert.Equal(t, 11, ce.ExitCode())
					assert.Equal(t, unsupportedRefusal(value, u), ce.Message)
					assert.Nil(t, ce.Details)
					refused, ok := errors.AsType[*unsupportedConfigRefused](err)
					require.True(t, ok, "the cause names the auth config")
					assert.Equal(t, unsupportedAuthConfig{ID: u.id, Type: u.typ}, refused.config)
					if withLocal {
						assert.Equal(t, []string{"local"}, candidateIDs(ce.Candidates))
					} else {
						assert.Empty(t, ce.Candidates)
					}
				})
			}
		}
	}
}

// TestAValueThatMatchesNoAuthConfigIsStillNotFound asserts the refusal is
// for a value that names an unsupported auth config: any other value that
// matches nothing is NOT_FOUND, with the supported auth configs as
// candidates.
func TestAValueThatMatchesNoAuthConfigIsStillNotFound(t *testing.T) {
	t.Parallel()
	srv := providerListServer(t, true)
	_, err := loginAndGenerateCred(t.Context(), srv.Client(), &loginInput{caFlag: "--cacert", server: srv.URL, authProvider: "no-such-config"}, noTerminal())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	assert.Equal(t, []string{"local"}, candidateIDs(ce.Candidates))
	_, hasCause := errors.AsType[*unsupportedConfigRefused](err)
	assert.False(t, hasCause)
}

// TestTheFirstOfSeveralUnsupportedMatchesIsNamed asserts a provider type
// that two auth configs of this CLI's unsupported types share names the first
// in the server's order.
func TestTheFirstOfSeveralUnsupportedMatchesIsNamed(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"eu-oidc","type":"oidcProvider"},{"id":"us-oidc","type":"oidcProvider"}]}`))
	}))
	t.Cleanup(srv.Close)
	_, err := loginAndGenerateCred(t.Context(), srv.Client(), &loginInput{caFlag: "--cacert", server: srv.URL, authProvider: "oidcProvider"}, noTerminal())
	refused, ok := errors.AsType[*unsupportedConfigRefused](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, unsupportedAuthConfig{ID: "eu-oidc", Type: "oidcProvider"}, refused.config)
}

// TestAnUnsupportedAuthConfigWithNoIDIsNamedByItsProviderName asserts the
// refusal names an unsupported auth config the server lists with no id by
// the provider name of its type.
func TestAnUnsupportedAuthConfigWithNoIDIsNamedByItsProviderName(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"type":"githubProvider"}]}`))
	}))
	t.Cleanup(srv.Close)
	_, err := loginAndGenerateCred(t.Context(), srv.Client(), &loginInput{caFlag: "--cacert", server: srv.URL, authProvider: "githubProvider"}, noTerminal())
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.True(t, strings.HasPrefix(ce.Message, "--auth-provider githubProvider names an auth config this CLI cannot sign in with: github (githubProvider); "), ce.Message)
}

// execRefusal runs the exec plugin for c against x and returns the refusal,
// and asserts what a kubectl exec entry needs of it: no result for stdout
// and no prompt, the code and exit status of CAPABILITY_UNSUPPORTED.
func execRefusal(t *testing.T, x *ownerFixture, c authCall) *clierr.Error {
	t.Helper()
	notices := &bytes.Buffer{}
	c.notices = notices
	res, err := x.executeWith(c)
	require.Error(t, err)
	assert.Zero(t, res, "nothing is written to stdout")
	assert.NotContains(t, notices.String(), "Enter ", "nothing is prompted")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
	assert.Equal(t, 11, ce.ExitCode())
	return ce
}

// allUnsupportedConfigs are the auth configs of unsupportedRows as the
// facade lists them.
func allUnsupportedConfigs() testrancher.Options {
	var opts testrancher.Options
	for _, u := range unsupportedRows {
		opts.AuthConfigs = append(opts.AuthConfigs, testrancher.AuthConfig{Name: u.id, Type: u.typ})
	}
	return opts
}

// TestEveryUnsupportedProviderTypeIsRefusedWithAPipedLoginByTheExecPlugin
// asserts the rule at the exec plugin, the way kubectl runs it with no
// terminal and with --no-input: for every provider type the server lists that
// this CLI cannot sign in with, --auth-provider as the auth config's id, its
// provider type and its provider name is CAPABILITY_UNSUPPORTED, exit 11,
// with the login that takes a piped API token as the hint, nothing for
// stdout, in the cluster-scoped form with token reuse off and on. Mutations:
// drop the case from withNoTerminalRemedies (every row fails on its hint).
func TestEveryUnsupportedProviderTypeIsRefusedWithAPipedLoginByTheExecPlugin(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		x := newOwnerFixture(t, reuse, allUnsupportedConfigs())
		login := clierr.Rancher + " auth login 'n' --url '" + x.serverID() + "' --token-stdin"
		for _, u := range unsupportedRows {
			for form, value := range u.values() {
				t.Run(u.typ+" by "+form+", token reuse "+map[bool]string{false: "off", true: "on"}[reuse], func(t *testing.T) {
					ce := execRefusal(t, x, authCall{cluster: "c-test", name: "n", authConfig: value, noInput: true})
					assert.Equal(t, unsupportedRefusal(value, u), ce.Message, "a named call carries the login as the hint")
					assert.Equal(t, login, ce.Hint)
				})
			}
		}
	}
}

// TestTheExecPluginRefusesAnUnsupportedAuthConfigInEveryForm asserts the
// refusal and its remedy in each form of the call: the cluster-scoped form
// and the form with --user-id, which names the auth config by its id only,
// each with --no-input and with no terminal, with token reuse off and on,
// and the legacy shim with no --cluster. A named call has the login as the
// hint; a call with no --name has it in the message and no hint, since a
// hint is a command that runs as written.
func TestTheExecPluginRefusesAnUnsupportedAuthConfigInEveryForm(t *testing.T) {
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCredentialStore, "")
	t.Setenv(execInfoEnv, "")
	for _, reuse := range []bool{false, true} {
		x := newOwnerFixture(t, reuse, testrancher.Options{ExtraProviders: []string{"githubProvider"}})
		login := clierr.Rancher + " auth login 'n' --url '" + x.serverID() + "' --token-stdin"
		unnamed := "; <name> stands for a name of your choice; to log in with an API token, pipe it to: " +
			clierr.Rancher + " auth login <name> --url '" + x.serverID() + "' --token-stdin"
		for _, noInput := range []bool{false, true} {
			for _, tc := range []struct {
				name string
				call authCall
			}{
				{"cluster-scoped", authCall{cluster: "c-test", name: "n", authConfig: "github"}},
				{"cluster-scoped, by provider type", authCall{cluster: "c-test", name: "n", authConfig: "githubProvider"}},
				{"--user-id", authCall{cluster: "c-test", name: "n", userID: "user-alice", authConfig: "github"}},
				{"--user-id with no cluster", authCall{name: "n", userID: "user-alice", authConfig: "github"}},
			} {
				t.Run(tc.name+", token reuse "+map[bool]string{false: "off", true: "on"}[reuse]+", --no-input "+map[bool]string{false: "off", true: "on"}[noInput], func(t *testing.T) {
					tc.call.noInput = noInput
					ce := execRefusal(t, x, tc.call)
					assert.True(t, strings.HasPrefix(ce.Message, "--auth-provider "+tc.call.authConfig+" names an auth config this CLI cannot sign in with: github (githubProvider); "), ce.Message)
					assert.Equal(t, login, ce.Hint)
					assert.NotContains(t, ce.Message, "kubectl", "a terminal does not make the auth config usable")
				})
			}
			t.Run("an unnamed call, token reuse "+map[bool]string{false: "off", true: "on"}[reuse], func(t *testing.T) {
				ce := execRefusal(t, x, authCall{cluster: "c-test", authConfig: "github", noInput: noInput})
				assert.Empty(t, ce.Hint, "the login names an entry of the operator's choice")
				assert.True(t, strings.HasSuffix(ce.Message, unnamed), ce.Message)
			})
		}
	}

	t.Run("a named call with an inline CA", func(t *testing.T) {
		pemCA := selfSignedCAPEM(t)
		for _, reuse := range []bool{false, true} {
			x := newOwnerFixture(t, reuse, testrancher.Options{ExtraProviders: []string{"githubProvider"}})
			ce := execRefusal(t, x, authCall{cluster: "c-test", name: "n", authConfig: "github", cacert: pemCA, noInput: true})
			assert.Empty(t, ce.Hint, "the login needs the CA from a file, which no hint can name")
			assert.True(t, strings.HasSuffix(ce.Message, "; this exec entry carries the CA inline and --cacert needs a file; "+config.CAFileMeaning+"; "+
				"to log in with an API token, pipe it to: "+clierr.Rancher+" auth login 'n' --url '"+x.serverID()+"' --cacert <file> --token-stdin"), "token reuse %t: %s", reuse, ce.Message)
			assert.NotContains(t, ce.Message, "-----BEGIN", "token reuse %t", reuse)
		}
	})

	t.Run("the legacy shim with no --cluster", func(t *testing.T) {
		srv := providerListServer(t, true)
		dir := t.TempDir()
		cmd := NewLegacyTokenCommand(dir, config.NewFileStore(dir), anchorFor(dir)).(runtime.ReadCommand)
		res, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": srv.URL, "user": "alice", "auth-provider": "githubProvider"}}, noTerminal())
		require.Error(t, err)
		assert.Zero(t, res)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "got %v", err)
		assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
		assert.Equal(t, 11, ce.ExitCode())
		assert.Empty(t, ce.Hint, "the shim's slot is no name auth login accepts")
		assert.True(t, strings.HasSuffix(ce.Message,
			"; <name> stands for a name of your choice; to log in with an API token, pipe it to: "+clierr.Rancher+" auth login <name> --url '"+config.CanonicalServerIdentity(srv.URL)+"' --token-stdin"), ce.Message)
	})
}

// TestAnUnsupportedAuthConfigBesideSupportedOnesLeavesTheirSignInAlone
// asserts that where the server lists an auth config this CLI cannot sign in
// with next to ones it can, a sign-in through a supported auth config still
// sends its name as configName (Rancher v2.16 needs it for LDAP) and the
// unsupported one is refused before any login request: with several auth
// configs of one supported type, as v2.16 lists, and with one per type.
func TestAnUnsupportedAuthConfigBesideSupportedOnesLeavesTheirSignInAlone(t *testing.T) {
	isolateSignIn(t)
	for _, tc := range []struct {
		name    string
		opts    testrancher.Options
		through string
	}{
		{"several auth configs of one supported type", testrancher.Options{AuthConfigs: []testrancher.AuthConfig{
			{Name: "openldap", Type: "openLdapProvider", Users: []testrancher.User{{Name: "alice", Password: "pw", UserID: "user-default"}}},
			{Name: "corp-ldap", Type: "openLdapProvider", Users: []testrancher.User{{Name: "alice", Password: "pw", UserID: "user-corp"}}},
			{Name: "corp-gh", Type: "githubProvider"},
		}}, "corp-ldap"},
		{"one auth config per type", testrancher.Options{
			AuthConfigs:    []testrancher.AuthConfig{{Name: "openldap", Type: "openLdapProvider", Users: []testrancher.User{{Name: "alice", Password: "pw", UserID: "user-default"}}}},
			ExtraProviders: []string{"githubProvider"},
		}, "openldap"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsupported := []string{"corp-gh", "githubProvider", "github"}
			if tc.through == "openldap" {
				unsupported = []string{"github", "githubProvider"}
			}
			for _, value := range unsupported {
				f, srv := newTokenFacade(t, tc.opts)
				_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{CAFlag: "--cacert", Server: srv.URL, AuthProvider: value, ResponseType: "json"}, 0, terminalLines("alice", "pw"))
				ce, ok := errors.AsType[*clierr.Error](err)
				require.True(t, ok, "%s: got %v", value, err)
				assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code, value)
				assert.Empty(t, f.LoginRecords(), "%s: refused before any login request", value)
			}
			f, srv := newTokenFacade(t, tc.opts)
			_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{CAFlag: "--cacert", Server: srv.URL, AuthProvider: tc.through, ResponseType: "json"}, 0, terminalLines("alice", "pw"))
			require.NoError(t, err)
			assert.Equal(t, []testrancher.LoginRecord{{Type: "openLdapProvider", ConfigName: tc.through, HasConfigName: true}}, f.LoginRecords())
		})
	}
}
