package token

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWireCompatibility(t *testing.T) {
	t.Run("Token unmarshals from Rancher API response", func(t *testing.T) {
		jsonResp := `{
			"token": "token-abc123",
			"expiresAt": "2026-07-04T12:00:00Z",
			"userId": "u-xyz",
			"authProvider": "local"
		}`

		var tok Token
		err := json.Unmarshal([]byte(jsonResp), &tok)
		require.NoError(t, err)
		assert.Equal(t, "token-abc123", tok.Token)
		assert.Equal(t, "2026-07-04T12:00:00Z", tok.ExpiresAt)
	})

	t.Run("AzureADProvider unmarshals from auth providers list", func(t *testing.T) {
		jsonResp := `{
			"type": "azureADProvider",
			"redirectUrl": "https://rancher.example.com/verify-auth-azure",
			"tenantId": "abc-def-ghi",
			"clientId": "client123",
			"scopes": ["openid", "profile", "email"],
			"authUrl": "https://login.microsoftonline.com/abc-def-ghi/oauth2/v2.0/authorize",
			"deviceAuthUrl": "https://login.microsoftonline.com/abc-def-ghi/oauth2/v2.0/devicecode",
			"tokenUrl": "https://login.microsoftonline.com/abc-def-ghi/oauth2/v2.0/token"
		}`

		var provider AzureADProvider
		err := json.Unmarshal([]byte(jsonResp), &provider)
		require.NoError(t, err)

		assert.Equal(t, "azureADProvider", provider.GetType())
		assert.Equal(t, "https://rancher.example.com/verify-auth-azure", provider.RedirectURL)
		assert.Equal(t, "abc-def-ghi", provider.TenantID)
		assert.Equal(t, "client123", provider.ClientID)
		assert.Equal(t, []string{"openid", "profile", "email"}, provider.Scopes)
		assert.Equal(t, "https://login.microsoftonline.com/abc-def-ghi/oauth2/v2.0/authorize", provider.AuthURL)
		assert.Equal(t, "https://login.microsoftonline.com/abc-def-ghi/oauth2/v2.0/devicecode", provider.DeviceAuthURL)
		assert.Equal(t, "https://login.microsoftonline.com/abc-def-ghi/oauth2/v2.0/token", provider.TokenURL)
	})

	t.Run("LocalProvider unmarshals from auth providers list", func(t *testing.T) {
		jsonResp := `{
			"type": "localProvider"
		}`

		var provider LocalProvider
		err := json.Unmarshal([]byte(jsonResp), &provider)
		require.NoError(t, err)
		assert.Equal(t, "localProvider", provider.GetType())
	})

	t.Run("AuthProvider base type unmarshals correctly", func(t *testing.T) {
		jsonResp := `{
			"type": "adfsProvider"
		}`

		var provider AuthProvider
		err := json.Unmarshal([]byte(jsonResp), &provider)
		require.NoError(t, err)
		assert.Equal(t, "adfsProvider", provider.GetType())
	})

	t.Run("typedProvider interface satisfied", func(t *testing.T) {
		var _ typedProvider = &AuthProvider{}
		var _ typedProvider = &LocalProvider{}
		var _ typedProvider = &AzureADProvider{}
	})
}

func TestParseLoginResponse(t *testing.T) {
	t.Parallel()

	t.Run("v3 token response", func(t *testing.T) {
		body := []byte(`{
			"type": "token",
			"token": "token-abc:secretxyz",
			"expiresAt": "2026-08-01T12:00:00Z",
			"userId": "user-123"
		}`)
		tok, err := parseLoginResponse(body)
		require.NoError(t, err)
		assert.Equal(t, "token-abc:secretxyz", tok.Token)
		assert.Equal(t, "2026-08-01T12:00:00Z", tok.ExpiresAt)
	})

	t.Run("ext token response", func(t *testing.T) {
		body := []byte(`{
			"apiVersion": "ext.cattle.io/v1",
			"kind": "Token",
			"metadata": {"name": "token-xyz"},
			"spec": {"userID": "user-123"},
			"status": {
				"bearerToken": "ext/token-xyz:secretabc",
				"expiresAt": "2026-08-01T12:00:00Z"
			}
		}`)
		tok, err := parseLoginResponse(body)
		require.NoError(t, err)
		assert.Equal(t, "ext/token-xyz:secretabc", tok.Token, "ext/ prefix must survive untouched (EXT-3)")
		assert.Equal(t, "2026-08-01T12:00:00Z", tok.ExpiresAt)
	})

	t.Run("unrecognized apiVersion with empty token is a descriptive error", func(t *testing.T) {
		body := []byte(`{
			"apiVersion": "ext.cattle.io/v2",
			"kind": "Token",
			"status": {}
		}`)
		_, err := parseLoginResponse(body)
		require.Error(t, err, "an unrecognized versioned response must not silently look like an empty-token auth failure")
		assert.ErrorContains(t, err, "ext.cattle.io/v2")
		assert.ErrorContains(t, err, "Token")
	})

	t.Run("stray apiVersion with non-Token kind parses as v3", func(t *testing.T) {
		body := []byte(`{
			"apiVersion": "management.cattle.io/v3",
			"kind": "somethingElse",
			"token": "tok:sec",
			"expiresAt": ""
		}`)
		tok, err := parseLoginResponse(body)
		require.NoError(t, err)
		assert.Equal(t, "tok:sec", tok.Token)
	})

	t.Run("ext response with empty bearer normalizes to empty Token", func(t *testing.T) {
		body := []byte(`{
			"apiVersion": "ext.cattle.io/v1",
			"kind": "Token",
			"status": {}
		}`)
		tok, err := parseLoginResponse(body)
		require.NoError(t, err, "empty bearer is not a parse error; token.go's empty-token guard owns it (EXT-6)")
		assert.Empty(t, tok.Token)
	})

	t.Run("invalid JSON errors", func(t *testing.T) {
		_, err := parseLoginResponse([]byte(`{not json`))
		require.Error(t, err)
	})
}
