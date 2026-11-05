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
