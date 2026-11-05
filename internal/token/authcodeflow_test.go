package token

import (
	"errors"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// authCodeLogin is the Azure AD browser login against the facade, which
// serves its identity provider stub on its own TLS origin. The login runs
// the code a sign-in with --auth-flow authcode runs, from the list of auth
// configs through the exchange of the authorization code to the login at
// Rancher; only the browser is a stub, the opener oauthAuthCodeAuth takes.
type authCodeLogin struct {
	f   *testrancher.Facade
	srv *httptest.Server
	// challenges are the code_challenge values of the authorization requests
	// the stub browser followed, as the CLI sent them.
	challenges []string
}

func newAuthCodeLogin(t *testing.T) *authCodeLogin {
	t.Helper()
	t.Setenv(config.EnvOAuthCallbackPort, "")
	t.Setenv(config.EnvLegacyOAuthCallbackPort, "")
	x := &authCodeLogin{f: testrancher.New(testrancher.Options{EnableIdP: true})}
	x.srv = httptest.NewTLSServer(x.f)
	t.Cleanup(x.srv.Close)
	return x
}

// browser is the stub browser: it follows the authorization URL to the
// authorize endpoint, which redirects to the callback on the loopback
// listener the CLI opened, and closes every body. tamper changes the query
// of the URL before it is followed.
func (x *authCodeLogin) browser(t *testing.T, tamper func(url.Values)) openBrowserFunc {
	t.Helper()
	return func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		q := u.Query()
		x.challenges = append(x.challenges, q.Get("code_challenge"))
		if tamper != nil {
			tamper(q)
			u.RawQuery = q.Encode()
		}
		resp, err := x.srv.Client().Get(u.String())
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}
}

// login runs the browser login as loginAndGenerateCred starts it: the auth
// config chosen from the server's list names the login.
func (x *authCodeLogin) login(t *testing.T, browser openBrowserFunc) (CreateResult, error) {
	t.Helper()
	providers, _, err := getAuthProviders(t.Context(), x.srv.Client(), x.srv.URL)
	require.NoError(t, err)
	provider, err := selectAuthProvider(t.Context(), providers, nil, "azureADProvider", false, ui.Discard())
	require.NoError(t, err)
	input := &loginInput{caFlag: "--cacert", server: x.srv.URL, idpClient: x.srv.Client()}
	input.authProvider = provider.GetType()
	input.configName = provider.GetID()
	input.authConfig = authConfigName(provider)
	return oauthAuthCodeAuth(t.Context(), x.srv.Client(), input, provider, 10*time.Second, browser, ui.Discard())
}

// TestAuthCodeFlowSignsInThroughTheAuthorizeEndpointWithItsVerifier asserts
// the browser login of an Azure AD auth config end to end against the
// facade: the authorization request carries the S256 challenge, the code the
// authorize endpoint redirects with is exchanged with the verifier that
// challenge came from, and the login at Rancher names the auth config.
func TestAuthCodeFlowSignsInThroughTheAuthorizeEndpointWithItsVerifier(t *testing.T) {
	x := newAuthCodeLogin(t)
	tok, err := x.login(t, x.browser(t, nil))
	require.NoError(t, err)
	assert.NotEmpty(t, tok.Bearer)

	require.Len(t, x.challenges, 1)
	verifiers := x.f.IdPVerifiers()
	require.Len(t, verifiers, 1, "the token endpoint saw one exchange of the code")
	assert.Equal(t, x.challenges[0], oauth2.S256ChallengeFromVerifier(verifiers[0]), "the verifier is the one the challenge came from")
	assert.Equal(t, []testrancher.LoginRecord{{Type: "azureADProvider", ConfigName: "azuread", HasConfigName: true}}, x.f.LoginRecords())
}

// TestAuthCodeFlowWithAChallengeThatIsNotTheVerifiersFailsTheExchange
// asserts that when the authorization request reaches the authorize
// endpoint with a code_challenge that is not the verifier's, the token
// endpoint refuses the code with invalid_grant, the login fails as AUTH with
// the identity provider's refusal, and nothing is sent to Rancher.
func TestAuthCodeFlowWithAChallengeThatIsNotTheVerifiersFailsTheExchange(t *testing.T) {
	x := newAuthCodeLogin(t)
	wrong := oauth2.S256ChallengeFromVerifier(oauth2.GenerateVerifier())
	_, err := x.login(t, x.browser(t, func(q url.Values) { q.Set("code_challenge", wrong) }))
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	assert.Contains(t, ce.Message, "failed to exchange authorization code for token: the identity provider refused the sign-in (400)")
	assert.Len(t, x.f.IdPVerifiers(), 1, "the token endpoint saw the verifier and refused it")
	assert.Empty(t, x.f.LoginRecords(), "no /v1-public/login request was sent")
}
