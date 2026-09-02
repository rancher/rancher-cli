package testrancher

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIdPStubIssuesIDTokenForAuthorizationCodeGrant pins the stub's
// authorization-code grant: any non-empty code yields an id_token the
// façade's login then accepts.
func TestIdPStubIssuesIDTokenForAuthorizationCodeGrant(t *testing.T) {
	t.Parallel()
	f := New(Options{EnableIdP: true})
	srv := httptest.NewServer(f.IdPHandler())
	t.Cleanup(srv.Close)
	resp, err := http.PostForm(srv.URL+"/idp/token", url.Values{"grant_type": {"authorization_code"}, "code": {"c-1"}})
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
