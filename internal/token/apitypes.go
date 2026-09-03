package token

import (
	"encoding/json"
	"fmt"
)

// Local type definitions extracted from github.com/rancher/rancher/pkg to avoid
// pulling in norman, fleet, wrangler, lasso, and the entire operator tree.
// These types are wire-compatible with the Rancher server API responses.

// Token represents an authentication token returned by Rancher login endpoints.
// Only the fields used by this package are defined.
type Token struct {
	Token     string `json:"token,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`

	// noDiskCache, when set, forbids writing this token to the on-disk
	// exec-credential cache: it marks a credential the backing store
	// guarantees only in memory (a credential-helper short-lived token served
	// through the legacy pre-ext.cattle.io fallback). It is never serialized.
	noDiskCache bool
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

// extToken is the minimal wire shape of an ext.cattle.io/v1 Token login
// response. Local definition per this file's no-rancher/rancher-import rule;
// upstream shape: rancher/rancher pkg/apis/ext.cattle.io/v1 (see also
// rancher/cli PR #632).
type extToken struct {
	Status struct {
		BearerToken string `json:"bearerToken"`
		ExpiresAt   string `json:"expiresAt"`
	} `json:"status"`
}

// parseLoginResponse normalizes a /v1-public/login response into Token,
// accepting both the v3 Norman shape (top-level token/expiresAt) and the
// ext.cattle.io/v1 Token shape (status.bearerToken/status.expiresAt) for the
// transition period while Rancher migrates session tokens. The ext bearer
// keeps its "ext/" prefix — the server authenticator parses it back out of
// the Bearer header, so it is opaque data here.
func parseLoginResponse(body []byte) (Token, error) {
	if kind, ok := LoginResponseShape(body); ok && kind == TokenKindExt {
		var et extToken
		if err := json.Unmarshal(body, &et); err != nil {
			return Token{}, fmt.Errorf("error unmarshaling ext token login response: %w", err)
		}
		return Token{Token: et.Status.BearerToken, ExpiresAt: et.Status.ExpiresAt}, nil
	}
	var tok Token
	if err := json.Unmarshal(body, &tok); err != nil {
		return Token{}, fmt.Errorf("error unmarshaling login response: %w", err)
	}
	if tok.Token == "" {
		var hdr struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
		}
		_ = json.Unmarshal(body, &hdr)
		if hdr.APIVersion != "" {
			return Token{}, fmt.Errorf("unsupported token API version %q (kind %q) in login response", hdr.APIVersion, hdr.Kind)
		}
	}
	return tok, nil
}
