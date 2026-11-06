package token

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// twoLDAPConfigs is a server with two OpenLDAP configs, each with its own
// user: the default one, named after its provider, and corp-ldap.
func twoLDAPConfigs() testrancher.Options {
	return testrancher.Options{AuthConfigs: []testrancher.AuthConfig{
		{Name: "openldap", Type: "openLdapProvider", Users: []testrancher.User{{Name: "alice", Password: "pw", UserID: "user-default"}}},
		{Name: "corp-ldap", Type: "openLdapProvider", Users: []testrancher.User{{Name: "alice", Password: "pw", UserID: "user-corp"}}},
	}}
}

// signInEntryPoint is one way a sign-in starts: auth login, auth get-token
// signing in to create a cluster token, auth get-token's plugin-created
// login with token reuse on, and the legacy token command.
type signInEntryPoint struct {
	name string
	run  func(t *testing.T, opts testrancher.Options, authProvider string, p runtime.Prompter) (*testrancher.Facade, error)
}

var signInEntryPoints = []signInEntryPoint{
	{"auth login", func(t *testing.T, opts testrancher.Options, authProvider string, p runtime.Prompter) (*testrancher.Facade, error) {
		t.Helper()
		f, srv := newTokenFacade(t, opts)
		_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{CAFlag: "--cacert", Server: srv.URL, AuthProvider: authProvider, ResponseType: "json"}, 0, p)
		return f, err
	}},
	{"get-token sign-in", func(t *testing.T, opts testrancher.Options, authProvider string, p runtime.Prompter) (*testrancher.Facade, error) {
		t.Helper()
		x := newPluginFixture(t, false, opts)
		_, err := x.command(x.store).Execute(t.Context(), tin{s: map[string]string{"server": x.url, "cluster": "c-test", "name": "n", "auth-provider": authProvider}}, p)
		return x.f, err
	}},
	{"get-token plugin login", func(t *testing.T, opts testrancher.Options, authProvider string, p runtime.Prompter) (*testrancher.Facade, error) {
		t.Helper()
		x := newPluginFixture(t, true, opts)
		_, err := x.command(x.store).Execute(t.Context(), tin{s: map[string]string{"server": x.url, "cluster": "c-test", "name": "n", "auth-provider": authProvider}}, p)
		return x.f, err
	}},
	{"legacy token", func(t *testing.T, opts testrancher.Options, authProvider string, p runtime.Prompter) (*testrancher.Facade, error) {
		t.Helper()
		x := newPluginFixture(t, false, opts)
		cmd := NewLegacyTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
		_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": x.url, "user": "alice", "auth-provider": authProvider}}, p)
		return x.f, err
	}},
}

// isolateSignIn clears the environment a sign-in reads and gives it a mock
// keychain.
func isolateSignIn(t *testing.T) {
	t.Helper()
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCredentialStore, "")
	t.Setenv(execInfoEnv, "")
	keyring.MockInit()
}

// TestEverySignInSendsTheChosenConfigName asserts each entry point's login
// request carries the id of the entry --auth-provider chose as configName,
// beside its type, and that the sign-in is as the user of that config.
func TestEverySignInSendsTheChosenConfigName(t *testing.T) {
	isolateSignIn(t)
	for _, ep := range signInEntryPoints {
		t.Run(ep.name, func(t *testing.T) {
			f, err := ep.run(t, twoLDAPConfigs(), "corp-ldap", terminalLines("alice", "pw"))
			require.NoError(t, err)
			assert.Equal(t, []testrancher.LoginRecord{{Type: "openLdapProvider", ConfigName: "corp-ldap", HasConfigName: true}}, f.LoginRecords())
		})
	}
}

// TestASignInThroughAnotherConfigIsAnotherUser asserts the login of a
// config signs in as that config's user, so the user id a sign-in records is
// the chosen config's.
func TestASignInThroughAnotherConfigIsAnotherUser(t *testing.T) {
	isolateSignIn(t)
	for authProvider, want := range map[string]string{"corp-ldap": "user-corp", "openldap": "user-default"} {
		f, srv := newTokenFacade(t, twoLDAPConfigs())
		cred, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{CAFlag: "--cacert", Server: srv.URL, AuthProvider: authProvider, ResponseType: "json"}, 0, terminalLines("alice", "pw"))
		require.NoError(t, err, authProvider)
		assert.Equal(t, want, cred.UserID, authProvider)
		assert.Len(t, f.LoginRecords(), 1)
	}
}

// TestTheConfigNamedLikeTheProviderIsPickedByItsName asserts --auth-provider
// openldap with the default config and another one enabled signs in
// through the config named openldap, and says which one it signs in
// through; a plain provider-name match would have asked.
func TestTheConfigNamedLikeTheProviderIsPickedByItsName(t *testing.T) {
	isolateSignIn(t)
	f, srv := newTokenFacade(t, twoLDAPConfigs())
	p := terminalLines("alice", "pw")
	_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{CAFlag: "--cacert", Server: srv.URL, AuthProvider: "openldap", ResponseType: "json"}, 0, p)
	require.NoError(t, err)
	assert.Equal(t, []testrancher.LoginRecord{{Type: "openLdapProvider", ConfigName: "openldap", HasConfigName: true}}, f.LoginRecords())
	require.GreaterOrEqual(t, p.at("Signing in through auth config openldap."), 0, "events %q", p.events)
	assert.Equal(t, "notice: Signing in through auth config openldap.", p.events[p.at("Signing in through")])
	assert.Zero(t, p.count("prompt: Select auth config"), "an exact name is not asked about")
}

// TestAProviderNameOfOneConfigSignsInThroughItWithoutANotice asserts
// --auth-provider openldap with only corp-ldap enabled signs in through it,
// and prints no sign-in notice, since the type has one config.
func TestAProviderNameOfOneConfigSignsInThroughItWithoutANotice(t *testing.T) {
	isolateSignIn(t)
	opts := testrancher.Options{AuthConfigs: twoLDAPConfigs().AuthConfigs[1:]}
	f, srv := newTokenFacade(t, opts)
	p := terminalLines("alice", "pw")
	_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{CAFlag: "--cacert", Server: srv.URL, AuthProvider: "openldap", ResponseType: "json"}, 0, p)
	require.NoError(t, err)
	assert.Equal(t, []testrancher.LoginRecord{{Type: "openLdapProvider", ConfigName: "corp-ldap", HasConfigName: true}}, f.LoginRecords())
	assert.Zero(t, p.count("Signing in through"), "events %q", p.events)
}

// TestATypeOfSeveralConfigsAsksAndSignsInThroughTheChoice asserts the type
// of two enabled configs asks, with a menu of the two, and the choice is the
// config the login names.
func TestATypeOfSeveralConfigsAsksAndSignsInThroughTheChoice(t *testing.T) {
	isolateSignIn(t)
	f, srv := newTokenFacade(t, twoLDAPConfigs())
	p := terminalLines("1", "alice", "pw")
	_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{CAFlag: "--cacert", Server: srv.URL, AuthProvider: "openLdapProvider", ResponseType: "json"}, 0, p)
	require.NoError(t, err)
	assert.Equal(t, "notice: Auth configs:\n0 - openldap\n1 - corp-ldap (openldap)\n", p.events[p.at("notice: Auth configs:")])
	assert.Equal(t, []testrancher.LoginRecord{{Type: "openLdapProvider", ConfigName: "corp-ldap", HasConfigName: true}}, f.LoginRecords())
	assert.Contains(t, p.events, "notice: Signing in through auth config corp-ldap (openldap).")
}

// TestTheSignInNoticeIsSaidOnlyWhenASignInFollows asserts the notice is not
// printed before a refusal for want of a terminal or under --no-input, and
// that the refusal records which of the two it is.
func TestTheSignInNoticeIsSaidOnlyWhenASignInFollows(t *testing.T) {
	isolateSignIn(t)
	for _, tc := range []struct {
		name        string
		p           runtime.Prompter
		wantNoInput bool
	}{
		{"no terminal", noTerminal(), false},
		{"no-input with a terminal", noInputWithTerminal(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, srv := newTokenFacade(t, twoLDAPConfigs())
			p := newEventPrompter(tc.p)
			_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{CAFlag: "--cacert", Server: srv.URL, AuthProvider: "corp-ldap", ResponseType: "json"}, 0, p)
			require.EqualError(t, err, "interactive login required for auth config corp-ldap")
			nt, ok := errors.AsType[*needsTerminal](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, tc.wantNoInput, nt.noInput)
			assert.Zero(t, p.count("Signing in through"), "events %q", p.events)
		})
	}
	t.Run("an OAuth config under --no-input", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			so := twoAzureConfigs(t)
			p := newEventPrompter(noInputWithTerminal())
			_, err := AcquireLoginCredential(t.Context(), so.client, LoginRequest{CAFlag: "--cacert", Server: so.url, AuthProvider: "corp-azure", AuthFlow: deviceAuthFlow, IdPClient: so.client, ResponseType: "json"}, 0, p)
			require.EqualError(t, err, "interactive login required for auth config corp-azure")
			nt, ok := errors.AsType[*needsTerminal](err)
			require.True(t, ok, "got %v", err)
			assert.True(t, nt.noInput)
			assert.Zero(t, p.count("Signing in through"), "events %q", p.events)
		})
	})
}

// TestEveryRefusalNamesTheAuthConfig asserts each call site of the sign-in
// refusal names the auth config, with the cause the exec plugin builds its
// remedies from: the password prompts, SAML under --no-input, and OAuth
// under --no-input, that the cause's own text names it too, and that the
// cause says whether --no-input is the reason. The message names no remedy:
// each command adds its own.
func TestEveryRefusalNamesTheAuthConfig(t *testing.T) {
	t.Parallel()
	const refusal = "interactive login required for auth config corp-ldap"
	basic := func() *loginInput {
		return &loginInput{caFlag: "--cacert", server: "https://r.invalid", authProvider: "openLdapProvider", authConfig: "corp-ldap"}
	}
	for _, tc := range []struct {
		name        string
		run         func(t *testing.T) error
		want        string
		wantNoInput bool
	}{
		{"no terminal", func(t *testing.T) error {
			t.Helper()
			_, err := basicAuth(t.Context(), failingTransportClient(t), basic(), noTerminal())
			return err
		}, refusal, false},
		{"the username refused", func(t *testing.T) error {
			t.Helper()
			_, err := basicAuth(t.Context(), failingTransportClient(t), basic(), usernameRefused{})
			return err
		}, refusal, false},
		{"the password refused", func(t *testing.T) error {
			t.Helper()
			_, err := basicAuth(t.Context(), failingTransportClient(t), basic(), usernameOnly{})
			return err
		}, refusal, false},
		{"SAML under --no-input", func(t *testing.T) error {
			t.Helper()
			srv := configsServer(t, [2]string{"okta", "oktaProvider"})
			_, err := loginAndGenerateCred(t.Context(), srv.Client(), &loginInput{caFlag: "--cacert", server: srv.URL}, ui.Discard())
			return err
		}, "interactive login required for auth config okta", true},
		{"OAuth under --no-input", func(t *testing.T) error {
			t.Helper()
			_, err := oauthAuth(t.Context(), failingTransportClient(t), &loginInput{caFlag: "--cacert", server: "https://r.invalid", authProvider: "azureADProvider", authConfig: "corp-azure"},
				&AzureADProvider{}, ui.Discard())
			return err
		}, "interactive login required for auth config corp-azure", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.run(t)
			require.EqualError(t, err, tc.want)
			nt, ok := errors.AsType[*needsTerminal](err)
			require.True(t, ok, "got %v", err)
			assert.Equal(t, "interactive login required for auth config "+nt.authConfig, nt.Error())
			assert.Equal(t, tc.wantNoInput, nt.noInput)
		})
	}
}

// usernameRefused can prompt and refuses the username prompt with
// ErrNoInput, as a terminal that closed does.
type usernameRefused struct{}

func (usernameRefused) Prompt(context.Context, string) (string, error) { return "", ui.ErrNoInput }
func (usernameRefused) PromptSecret(context.Context, string) (string, error) {
	return "", ui.ErrNoInput
}
func (usernameRefused) ReadLine(context.Context) (string, error) { return "", ui.ErrNoInput }
func (usernameRefused) Notice(string)                            {}
func (usernameRefused) CanPrompt() bool                          { return true }
func (usernameRefused) NoInput() bool                            { return false }

// configsServer is a Rancher that lists the given auth configs, as an id
// and a type each, and answers no login.
func configsServer(t *testing.T, entries ...[2]string) *httptest.Server {
	t.Helper()
	var items []map[string]string
	for _, e := range entries {
		items = append(items, map[string]string{"id": e[0], "type": e[1]})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			writeTestJSON(w, http.StatusOK, map[string]any{"data": items})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestASAMLConfigOfSeveralSaysTheBrowserPageChoosesItself asserts that after
// a SAML config is chosen from several SAML configs the sign-in says the
// browser page offers its own choice, before the request id, and prints no
// notice naming the config, since the CLI cannot pass it on.
func TestASAMLConfigOfSeveralSaysTheBrowserPageChoosesItself(t *testing.T) {
	isolateSignIn(t)
	restore := SetSAMLTimingForTest(50*time.Millisecond, 10*time.Millisecond)
	t.Cleanup(restore)
	const notice = "The Rancher sign-in page in your browser offers its own choice of auth config; the CLI cannot pass yours to it."

	srv := configsServer(t, [2]string{"okta", "oktaProvider"}, [2]string{"okta-eu", "oktaProvider"}, [2]string{"local", "localProvider"})
	p := newEventPrompter(noTerminal())
	_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{CAFlag: "--cacert", Server: srv.URL, AuthProvider: "okta-eu", ResponseType: "json"}, 0, p)
	require.ErrorContains(t, err, "timed out")
	require.GreaterOrEqual(t, p.at(notice), 0, "events %q", p.events)
	assert.Less(t, p.at(notice), p.at("Login Request Id"), "events %q", p.events)
	assert.Zero(t, p.count("Signing in through"), "events %q", p.events)

	one := configsServer(t, [2]string{"okta", "oktaProvider"}, [2]string{"local", "localProvider"})
	p = newEventPrompter(noTerminal())
	_, err = AcquireLoginCredential(t.Context(), one.Client(), LoginRequest{CAFlag: "--cacert", Server: one.URL, AuthProvider: "okta", ResponseType: "json"}, 0, p)
	require.ErrorContains(t, err, "timed out")
	assert.Zero(t, p.count(notice), "one SAML config: nothing to say; events %q", p.events)

	under := newEventPrompter(ui.Discard())
	_, err = AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{CAFlag: "--cacert", Server: srv.URL, AuthProvider: "okta-eu", ResponseType: "json"}, 0, under)
	require.EqualError(t, err, "interactive login required for auth config okta-eu")
	assert.Zero(t, under.count(notice), "no notice before a --no-input refusal; events %q", under.events)
}

// TestAGenericSAMLConfigCountsAmongTheSAMLConfigsTheBrowserPageOffers asserts
// that a SAML sign-in through an okta config says the browser page offers its
// own choice when a Generic SAML config is also listed, and the same for a
// sign-in through the Generic SAML config: the page offers every SAML config
// of the server, whatever its type.
func TestAGenericSAMLConfigCountsAmongTheSAMLConfigsTheBrowserPageOffers(t *testing.T) {
	isolateSignIn(t)
	restore := SetSAMLTimingForTest(50*time.Millisecond, 10*time.Millisecond)
	t.Cleanup(restore)
	const notice = "The Rancher sign-in page in your browser offers its own choice of auth config; the CLI cannot pass yours to it."

	srv := configsServer(t, [2]string{"okta", "oktaProvider"}, [2]string{"corp-saml", "genericSAMLProvider"}, [2]string{"local", "localProvider"})
	for _, chosen := range []string{"okta", "corp-saml"} {
		p := newEventPrompter(noTerminal())
		_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{CAFlag: "--cacert", Server: srv.URL, AuthProvider: chosen, ResponseType: "json"}, 0, p)
		require.ErrorContains(t, err, "timed out", chosen)
		require.GreaterOrEqual(t, p.at(notice), 0, "%s: events %q", chosen, p.events)
		assert.Less(t, p.at(notice), p.at("Login Request Id"), "%s: events %q", chosen, p.events)
	}

	one := configsServer(t, [2]string{"corp-saml", "genericSAMLProvider"}, [2]string{"local", "localProvider"})
	p := newEventPrompter(noTerminal())
	_, err := AcquireLoginCredential(t.Context(), one.Client(), LoginRequest{CAFlag: "--cacert", Server: one.URL, AuthProvider: "corp-saml", ResponseType: "json"}, 0, p)
	require.ErrorContains(t, err, "timed out")
	assert.Zero(t, p.count(notice), "one SAML config: nothing to say; events %q", p.events)
}

// loginBodies is a Rancher whose provider list is configs and whose login
// records every request body and answers 401.
type loginBodies struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
}

func newLoginBodies(t *testing.T, entries ...map[string]string) *loginBodies {
	t.Helper()
	lb := &loginBodies{}
	lb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1-public/authproviders"):
			writeTestJSON(w, http.StatusOK, map[string]any{"data": entries})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1-public/login"):
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			lb.mu.Lock()
			lb.bodies = append(lb.bodies, body)
			lb.mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(lb.srv.Close)
	return lb
}

func (lb *loginBodies) sent() []map[string]any {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return append([]map[string]any(nil), lb.bodies...)
}

// TestAnEntryWithNoIDSendsNoConfigName asserts a login through an entry the
// server lists with no id carries the type and no configName at all, at
// every entry point.
func TestAnEntryWithNoIDSendsNoConfigName(t *testing.T) {
	isolateSignIn(t)
	run := func(t *testing.T, lb *loginBodies, name string, p runtime.Prompter) {
		t.Helper()
		switch name {
		case "auth login":
			_, err := AcquireLoginCredential(t.Context(), lb.srv.Client(), LoginRequest{CAFlag: "--cacert", Server: lb.srv.URL, ResponseType: "json"}, 0, p)
			require.Error(t, err)
		case "legacy token":
			dir := t.TempDir()
			cmd := NewLegacyTokenCommand(dir, config.NewFileStore(dir), anchorFor(dir)).(runtime.ReadCommand)
			_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": lb.srv.URL, "user": "alice"}}, p)
			require.Error(t, err)
		default:
			dir := t.TempDir()
			cfg := &config.Config{Servers: map[string]config.Server{}, TokenReuse: name == "get-token plugin login", CredentialStore: "file"}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())
			cmd := tokenCmd{dir: dir, store: config.NewFileStore(dir), anchor: anchorFor(dir)}
			_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": lb.srv.URL, "cluster": "c-1", "name": "n"}}, p)
			require.Error(t, err)
		}
	}
	for _, ep := range signInEntryPoints {
		t.Run(ep.name, func(t *testing.T) {
			lb := newLoginBodies(t, map[string]string{"type": "localProvider"})
			run(t, lb, ep.name, terminalLines("alice", "pw"))
			sent := lb.sent()
			require.Len(t, sent, 1)
			assert.Equal(t, "localProvider", sent[0]["type"])
			assert.NotContains(t, sent[0], "configName")
		})
	}
}

// multiAzure is a Rancher on the in-memory network that lists two Azure AD
// configs, each with its own client and endpoints, and signs in through the
// identity provider stub it hosts.
type multiAzure struct {
	f      *testrancher.Facade
	client *http.Client
	url    string
}

func twoAzureConfigs(t *testing.T) *multiAzure {
	t.Helper()
	f, rt := newInMemoryTokenFacade(t, testrancher.Options{EnableIdP: true, AuthConfigs: []testrancher.AuthConfig{
		{Name: "corp-azure", Type: "azureADProvider", Users: []testrancher.User{{Name: "corp", Password: "x", UserID: "user-corp"}}},
		{Name: "eu-azure", Type: "azureADProvider", Users: []testrancher.User{{Name: "eu", Password: "x", UserID: "user-eu"}}},
	}})
	return &multiAzure{f: f, client: rt.client(), url: rancherOrigin}
}

// TestTwoAzureConfigsUseTheChosenOnesClientEndpointsAndName asserts the
// OAuth sign-in takes the client, the endpoints and the config name from the
// entry that was chosen, never from another Azure AD entry, and signs in as
// that config's user.
func TestTwoAzureConfigsUseTheChosenOnesClientEndpointsAndName(t *testing.T) {
	for _, chosen := range []struct{ name, user string }{{"eu-azure", "user-eu"}, {"corp-azure", "user-corp"}} {
		synctest.Test(t, func(t *testing.T) {
			so := twoAzureConfigs(t)
			cred, err := AcquireLoginCredential(t.Context(), so.client, LoginRequest{
				CAFlag: "--cacert",
				Server: so.url, AuthProvider: chosen.name, AuthFlow: deviceAuthFlow, IdPClient: so.client, ResponseType: "json",
			}, 0, interactivePrompter())
			require.NoError(t, err)
			assert.Equal(t, chosen.user, cred.UserID)
			assert.Equal(t, []testrancher.IdPRequest{{Path: "/idp/" + chosen.name + "/devicecode", ClientID: "facade-client-" + chosen.name}}, so.f.IdPRequests())
			assert.Equal(t, []testrancher.LoginRecord{{Type: "azureADProvider", ConfigName: chosen.name, HasConfigName: true}}, so.f.LoginRecords())
		})
	}
}

// TestAnAzureSignInOfTheGetTokenLoginsSendsTheConfigName asserts the OAuth
// path's login request carries configName for the plugin-created login and
// for the direct sign-in of get-token, which share the sign-in with auth
// login.
func TestAnAzureSignInOfTheGetTokenLoginsSendsTheConfigName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		so := twoAzureConfigs(t)
		input := &loginInput{caFlag: "--cacert", server: so.url, authProvider: "eu-azure", authFlow: deviceAuthFlow, idpClient: so.client, responseType: "json"}
		res, err := loginAndGenerateCred(t.Context(), so.client, input, interactivePrompter())
		require.NoError(t, err)
		assert.Equal(t, CreateKnown, res.Create.Outcome)
		assert.Equal(t, []testrancher.LoginRecord{{Type: "azureADProvider", ConfigName: "eu-azure", HasConfigName: true}}, so.f.LoginRecords())
	})
}

// loginAnswering is a Rancher with the local auth config whose login answers
// status with body.
func loginAnswering(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1-public/authproviders"):
			writeTestJSON(w, http.StatusOK, map[string]any{"data": []map[string]string{{"id": "local", "type": "localProvider"}}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1-public/login"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAnUnusableTokenAfterALoginPointsAtTheAuthConfig asserts the two
// messages for a login that created no usable token - auth login's and the
// legacy token command's - tell the operator to check the auth config and
// credentials.
func TestAnUnusableTokenAfterALoginPointsAtTheAuthConfig(t *testing.T) {
	isolateSignIn(t)
	t.Run("auth login", func(t *testing.T) {
		srv := loginAnswering(t, http.StatusCreated, `{}`)
		_, err := AcquireLoginCredential(t.Context(), srv.Client(), LoginRequest{CAFlag: "--cacert", Server: srv.URL, ResponseType: "json"}, 0, terminalLines("alice", "pw"))
		require.EqualError(t, err, "authentication returned no usable token; check the auth config and credentials")
	})
	t.Run("legacy token", func(t *testing.T) {
		srv := loginAnswering(t, http.StatusCreated, `{}`)
		dir := t.TempDir()
		cmd := NewLegacyTokenCommand(dir, config.NewFileStore(dir), anchorFor(dir)).(runtime.ReadCommand)
		_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": srv.URL, "user": "alice"}}, terminalLines("alice", "pw"))
		require.EqualError(t, err, "authentication returned an empty token; check the auth config and credentials")
	})
}
