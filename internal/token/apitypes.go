package token

// Local type definitions extracted from github.com/rancher/rancher/pkg to avoid
// pulling in norman, fleet, wrangler, lasso, and the entire operator tree.
// These types are wire-compatible with the Rancher server API responses.

// Token represents an authentication token returned by Rancher login endpoints.
// Only the fields used by this package are defined.
type Token struct {
	Token     string `json:"token,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// AuthProvider is the base provider type; GetType satisfies the typedProvider interface.
type AuthProvider struct {
	Type string `json:"type"`
}

func (a *AuthProvider) GetType() string {
	return a.Type
}

// OAuthEndpoint contains the endpoints needed for an OAuth exchange.
type OAuthEndpoint struct {
	AuthURL       string `json:"authUrl,omitempty"`
	DeviceAuthURL string `json:"deviceAuthUrl,omitempty"`
	TokenURL      string `json:"tokenUrl,omitempty"`
}

// OAuthProvider contains the OAuth configuration of the AuthProvider.
type OAuthProvider struct {
	ClientID      string   `json:"clientId"`
	Scopes        []string `json:"scopes"`
	OAuthEndpoint `json:",inline"`
}

// AzureADProvider represents an Azure AD authentication provider.
type AzureADProvider struct {
	AuthProvider  `json:",inline"`
	RedirectURL   string `json:"redirectUrl"`
	TenantID      string `json:"tenantId,omitempty"`
	OAuthProvider `json:",inline"`
}

// LocalProvider represents the local authentication provider.
type LocalProvider struct {
	AuthProvider `json:",inline"`
}
