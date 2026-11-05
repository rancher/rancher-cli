package testrancher

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIdPStubIssuesIDTokenForAuthorizationCodeGrant asserts the stub's
// authorization-code grant: any non-empty code yields an id_token the
// facade's login then accepts.
func TestIdPStubIssuesIDTokenForAuthorizationCodeGrant(t *testing.T) {
	t.Parallel()
	f := New(Options{EnableIdP: true})
	srv := httptest.NewServer(f.IdPHandler())
	t.Cleanup(srv.Close)
	resp, err := srv.Client().PostForm(srv.URL+"/idp/token", url.Values{"grant_type": {"authorization_code"}, "code": {"c-1"}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body struct {
		IDToken string `json:"id_token"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.NotEmpty(t, body.IDToken)
	assert.True(t, f.idp.validIDToken(body.IDToken))
}

// noRedirects is a client that returns a redirect as the answer instead of
// following it.
func noRedirects(srv *httptest.Server) *http.Client {
	c := *srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// authorizeQuery is a valid authorization request for the challenge of
// verifier.
func authorizeQuery(verifier string) url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {"facade-client"},
		"redirect_uri":          {"http://localhost:5555/callback"},
		"state":                 {"st-1"},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}
}

// authorizeFor sends a valid authorization request for the challenge of
// verifier to path and returns the code of the redirect it answers with.
func authorizeFor(t *testing.T, srv *httptest.Server, path, verifier string) string {
	t.Helper()
	resp, err := noRedirects(srv).Get(srv.URL + path + "?" + authorizeQuery(verifier).Encode())
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusFound, resp.StatusCode)
	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "localhost:5555", loc.Host)
	assert.Equal(t, "/callback", loc.Path)
	assert.Equal(t, "st-1", loc.Query().Get("state"))
	code := loc.Query().Get("code")
	require.NotEmpty(t, code)
	return code
}

// exchange posts the authorization-code grant for code and verifier to the
// token endpoint at path and returns the status and the error of the answer.
func exchange(t *testing.T, srv *httptest.Server, path, code, verifier string) (int, string) {
	t.Helper()
	resp, err := srv.Client().PostForm(srv.URL+path, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return resp.StatusCode, body.Error
}

// TestIdPStubAuthorizeIssuesACodeBoundToItsChallenge asserts the authorize
// endpoint redirects a valid request to its redirect_uri with a code and the
// same state, at the default path and under the path of each Azure AD auth
// config, and that the token endpoint accepts that code once, with the
// verifier of the challenge it was issued for and no other, and records
// every verifier it was sent.
func TestIdPStubAuthorizeIssuesACodeBoundToItsChallenge(t *testing.T) {
	t.Parallel()
	const verifier = "v-0123456789012345678901234567890123456789012"
	f := New(Options{EnableIdP: true, AuthConfigs: []AuthConfig{{Name: "corp-azure", Type: "azureADProvider"}}})
	for name, h := range map[string]http.Handler{"the facade": f, "the identity provider's own origin": f.IdPHandler()} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			for _, prefix := range []string{"/idp", "/idp/corp-azure"} {
				code := authorizeFor(t, srv, prefix+"/authorize", verifier)
				status, errCode := exchange(t, srv, prefix+"/token", code, verifier)
				assert.Equal(t, http.StatusOK, status, prefix)
				assert.Empty(t, errCode, prefix)

				status, errCode = exchange(t, srv, prefix+"/token", code, verifier)
				assert.Equal(t, http.StatusBadRequest, status, "%s: a code is exchanged once", prefix)
				assert.Equal(t, "invalid_grant", errCode, prefix)
			}
		})
	}
	before := len(f.IdPVerifiers())
	srv := httptest.NewServer(f.IdPHandler())
	t.Cleanup(srv.Close)
	code := authorizeFor(t, srv, "/idp/authorize", verifier)
	status, errCode := exchange(t, srv, "/idp/token", code, "another-verifier")
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "invalid_grant", errCode)
	status, _ = exchange(t, srv, "/idp/token", "code-nobody-issued", "")
	assert.Equal(t, http.StatusOK, status, "a code the authorize endpoint did not issue keeps the stub's earlier behavior")
	got := f.IdPVerifiers()
	require.Len(t, got, before+1, "an unissued code records no verifier")
	assert.Equal(t, "another-verifier", got[len(got)-1])
}

// TestIdPStubAuthorizeRefusesARequestThatLacksAParameter asserts each
// parameter the authorize endpoint checks is checked: response_type=code, a
// client_id, an http or https redirect_uri, a state, a code_challenge and the
// S256 method. Each is a 400 and issues no code.
func TestIdPStubAuthorizeRefusesARequestThatLacksAParameter(t *testing.T) {
	t.Parallel()
	f := New(Options{EnableIdP: true})
	srv := httptest.NewServer(f.IdPHandler())
	t.Cleanup(srv.Close)
	for _, tc := range []struct {
		name   string
		change func(url.Values)
	}{
		{"no response_type", func(q url.Values) { q.Del("response_type") }},
		{"response_type token", func(q url.Values) { q.Set("response_type", "token") }},
		{"no client_id", func(q url.Values) { q.Del("client_id") }},
		{"no redirect_uri", func(q url.Values) { q.Del("redirect_uri") }},
		{"a redirect_uri that is not http", func(q url.Values) { q.Set("redirect_uri", "ftp://localhost/callback") }},
		{"no state", func(q url.Values) { q.Del("state") }},
		{"no code_challenge", func(q url.Values) { q.Del("code_challenge") }},
		{"no code_challenge_method", func(q url.Values) { q.Del("code_challenge_method") }},
		{"code_challenge_method plain", func(q url.Values) { q.Set("code_challenge_method", "plain") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := authorizeQuery("v")
			tc.change(q)
			resp, err := noRedirects(srv).Get(srv.URL + "/idp/authorize?" + q.Encode())
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.Copy(io.Discard, resp.Body)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			assert.Empty(t, resp.Header.Get("Location"))
		})
	}
}

// TestIdPStubTakesAGetOnlyAtTheAuthorizePath asserts the identity-provider
// routes keep their methods: authorize is a GET and the device-code and token
// endpoints are POSTs, on the facade and on the identity provider's own
// origin, so the one GET the stub serves opens no other route.
func TestIdPStubTakesAGetOnlyAtTheAuthorizePath(t *testing.T) {
	t.Parallel()
	f := New(Options{EnableIdP: true})
	for name, h := range map[string]http.Handler{"the facade": f, "the identity provider's own origin": f.IdPHandler()} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			for _, tc := range []struct {
				method, path string
			}{
				{http.MethodGet, "/idp/token"},
				{http.MethodGet, "/idp/devicecode"},
				{http.MethodPost, "/idp/authorize"},
			} {
				req, err := http.NewRequestWithContext(t.Context(), tc.method, srv.URL+tc.path, nil)
				require.NoError(t, err)
				resp, err := srv.Client().Do(req)
				require.NoError(t, err)
				_ = resp.Body.Close()
				assert.Equal(t, http.StatusNotFound, resp.StatusCode, "%s %s", tc.method, tc.path)
			}
		})
	}
}
