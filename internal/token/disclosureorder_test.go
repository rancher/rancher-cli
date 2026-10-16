package token

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// plaintextDisclosureMark is a word of the plaintext-file disclosure no
// other notice carries.
const plaintextDisclosureMark = "credentials.yaml"

// serverOf is a server constructor for a table row: a Rancher that
// advertises the given auth provider types.
func serverOf(types ...string) func(t *testing.T) *httptest.Server {
	return func(t *testing.T) *httptest.Server {
		t.Helper()
		return providersServer(t, types...)
	}
}

// refusingServer answers the auth provider list with a server error.
func refusingServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// deviceFailsServer is a Rancher that lists an azureADProvider whose
// identity provider is on a TLS server this process does not trust, so the
// device-authorization request fails before anyone is asked to act.
func deviceFailsServer(t *testing.T) *httptest.Server {
	t.Helper()
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	t.Cleanup(idp.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1-public/authproviders") {
			writeTestJSON(w, http.StatusOK, map[string]any{"data": []map[string]any{{
				"type": "azureADProvider", "clientId": "c", "scopes": []string{"openid"},
				"authUrl": idp.URL + "/authorize", "deviceAuthUrl": idp.URL + "/devicecode", "tokenUrl": idp.URL + "/token",
			}}})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAPluginSignInRefusedBeforeItsFirstPromptDisclosesNothing asserts the
// plaintext-file disclosure is not printed for a sign-in that is refused
// before it asks anyone to act, with token reuse on and the file store:
// nothing is signed in, so there is nothing to disclose.
func TestAPluginSignInRefusedBeforeItsFirstPromptDisclosesNothing(t *testing.T) {
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCredentialStore, "")
	t.Setenv(execInfoEnv, "")
	keyring.MockInit()
	for _, tc := range []struct {
		name    string
		server  func(t *testing.T) *httptest.Server
		noInput bool
		extra   map[string]string
		want    string
	}{
		{name: "several providers and no --auth-provider",
			server: serverOf("localProvider", "oktaProvider"),
			want:   "this server has several auth configs and this call has no terminal to choose one"},
		{name: "a basic provider and no terminal",
			server: serverOf("localProvider"),
			want:   "interactive login required for auth config local"},
		{name: "the provider list refused",
			server: refusingServer, want: "error listing auth providers"},
		{name: "SAML under --no-input",
			server:  serverOf("oktaProvider"),
			noInput: true, want: "interactive login required for auth config okta"},
		{name: "the device-authorization request failing",
			server: deviceFailsServer, extra: map[string]string{"auth-provider": "azureADProvider"},
			want: "failed to initiate device authorization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var errw bytes.Buffer
			p := ui.New(&errw, strings.NewReader(""), tc.noInput, false)
			err := runProviderChoice(t, tc.server(t), "plugin login", p, tc.extra, false)
			require.ErrorContains(t, err, tc.want)
			assert.NotContains(t, errw.String(), plaintextDisclosureMark)
		})
	}
}

// TestAPluginSignInWithATerminalDisclosesBeforeItsFirstPrompt asserts the
// disclosure is printed once, right before the username prompt, and after
// the provider menu and its choice when the server has several providers;
// the sign-in then succeeds and commits.
func TestAPluginSignInWithATerminalDisclosesBeforeItsFirstPrompt(t *testing.T) {
	keyring.MockInit()
	t.Run("one provider", func(t *testing.T) {
		x := newPluginFixture(t, true, testrancher.Options{})
		p := terminalLines("e2e-user", "e2e-pass")
		_, err := x.getToken(t, "c-test", "n", p)
		require.NoError(t, err)
		assert.Equal(t, 1, p.count(plaintextDisclosureMark), "events %q", p.events)
		assert.Less(t, p.at(plaintextDisclosureMark), p.at("prompt: Enter username"), "events %q", p.events)
		_, ok := x.entry(t)
		assert.True(t, ok, "the login committed")
	})
	t.Run("several providers", func(t *testing.T) {
		x := newPluginFixture(t, true, testrancher.Options{ExtraProviders: []string{"openLdapProvider"}})
		p := terminalLines("0", "e2e-user", "e2e-pass")
		_, err := x.command(x.store).Execute(t.Context(), tin{s: map[string]string{"server": x.url, "cluster": "c-test", "name": "n"}}, p)
		require.NoError(t, err)
		assert.Equal(t, 1, p.count(plaintextDisclosureMark), "events %q", p.events)
		assert.Less(t, p.at("notice: Auth configs:"), p.at("prompt: Select auth config"), "events %q", p.events)
		assert.Less(t, p.at("prompt: Select auth config"), p.at(plaintextDisclosureMark), "events %q", p.events)
		assert.Less(t, p.at(plaintextDisclosureMark), p.at("prompt: Enter username"), "events %q", p.events)
		_, ok := x.entry(t)
		assert.True(t, ok, "the login committed")
	})
}

// TestAPluginSignInNamesItsConfigBeforeTheDisclosure asserts that when the
// type has several auth configs the notice naming the one signed in through
// comes after the choice and before the plaintext-file disclosure, which is
// before the first prompt.
func TestAPluginSignInNamesItsConfigBeforeTheDisclosure(t *testing.T) {
	isolateSignIn(t)
	x := newPluginFixture(t, true, twoLDAPConfigs())
	p := terminalLines("alice", "pw")
	_, err := x.command(x.store).Execute(t.Context(), tin{s: map[string]string{"server": x.url, "cluster": "c-test", "name": "n", "auth-provider": "corp-ldap"}}, p)
	require.NoError(t, err)
	require.Equal(t, 1, p.count(plaintextDisclosureMark), "events %q", p.events)
	require.GreaterOrEqual(t, p.at("Signing in through auth config corp-ldap"), 0, "events %q", p.events)
	assert.Less(t, p.at("Signing in through auth config corp-ldap"), p.at(plaintextDisclosureMark), "events %q", p.events)
	assert.Less(t, p.at(plaintextDisclosureMark), p.at("prompt: Enter username"), "events %q", p.events)
}

// TestAPluginSAMLSignInDisclosesBeforeItsRequestID asserts that with no
// terminal, where the sign-in is a page to open, the disclosure comes right
// before the notice that asks for it.
func TestAPluginSAMLSignInDisclosesBeforeItsRequestID(t *testing.T) {
	t.Setenv(config.EnvToken, "")
	t.Setenv(config.EnvURL, "")
	t.Setenv(config.EnvCredentialStore, "")
	t.Setenv(execInfoEnv, "")
	keyring.MockInit()
	restore := SetSAMLTimingForTest(50*time.Millisecond, 10*time.Millisecond)
	t.Cleanup(restore)
	srv := providersServer(t, "localProvider", "oktaProvider")
	p := newEventPrompter(noTerminal())
	err := runProviderChoice(t, srv, "plugin login", p, map[string]string{"auth-provider": "oktaProvider"}, false)
	require.ErrorContains(t, err, "timed out")
	assert.Equal(t, 1, p.count(plaintextDisclosureMark), "events %q", p.events)
	assert.Less(t, p.at(plaintextDisclosureMark), p.at("Login Request Id"), "events %q", p.events)
}

// TestAPluginOAuthDeviceSignInDisclosesBeforeItsCode asserts the same for
// the device-code sign-in: the disclosure comes after the device
// authorization answered and right before the notice that carries the code.
func TestAPluginOAuthDeviceSignInDisclosesBeforeItsCode(t *testing.T) {
	keyring.MockInit()
	t.Setenv(config.EnvCredentialStore, "")
	so := splitOriginFacade(t)
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{}, TokenReuse: true, CredentialStore: "file"}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	cmd := tokenCmd{dir: dir, store: config.NewFileStore(dir), anchor: anchorFor(dir)}
	p := newEventPrompter(interactivePrompter())
	_, _, err := cmd.pluginLogin(t.Context(), pluginLoginInput{
		server: so.rancherURL, serverID: config.CanonicalServerIdentity(so.rancherURL), clusterID: "c-1", client: so.rancherClient,
		authProvider: "azureADProvider", authFlow: deviceAuthFlow, wantName: "n",
		newIdPClient: func() *http.Client { return so.idpClient },
	}, runtime.Prompter(p))
	require.Error(t, err, "the fixture has no clusters, so the login stops after the sign-in")
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	details, ok := ce.Details.(clierr.LoginFailureDetails)
	require.True(t, ok, "details %T", ce.Details)
	assert.Equal(t, clierr.StageClusterToken, details.Stage, "the sign-in finished; the cluster token step failed")
	assert.Equal(t, 1, p.count(plaintextDisclosureMark), "events %q", p.events)
	require.GreaterOrEqual(t, p.at("To sign in, use a web browser"), 0, "the sign-in reached its code: %q", p.events)
	assert.Less(t, p.at(plaintextDisclosureMark), p.at("To sign in, use a web browser"), "events %q", p.events)
}
