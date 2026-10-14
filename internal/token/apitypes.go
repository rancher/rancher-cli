package token

import (
	"github.com/rancher/rancher-cli/internal/config"
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
	// fresh is set when this invocation created the token: the direct
	// endpoint wait runs for it and cancellation revokes it. Never
	// serialized.
	fresh *freshToken
	// binding is what a cache record for this credential is written with:
	// the config entry it belongs to and that entry's cache binding id,
	// both read in the same config load that produced the credential. It
	// travels with the token rather than being looked up at the cache
	// write, where a second load could pair this credential with the id of
	// a login that replaced it since. Never serialized.
	binding config.CacheBinding
}

// recordBinding is the binding the token's cache record is written under:
// the token's own binding, naming the token and its owner when this
// invocation created it, so a later delete of the record can revoke it. A
// token the invocation did not create - the stored login served as is -
// is never named.
func (t Token) recordBinding() config.CacheBinding {
	if t.fresh == nil || t.fresh.create.Outcome != CreateKnown || t.fresh.create.Ref == nil {
		return t.binding
	}
	return t.binding.WithCreatedToken(t.fresh.create.Ref, t.fresh.create.UserID)
}

// AuthProvider is the base provider type; GetType satisfies the typedProvider interface.
type AuthProvider struct {
	Type string `json:"type"`
	// ID is the provider's short name in the server's list ("local",
	// "github"); accepted for --auth-provider beside the type.
	ID string `json:"id,omitempty"`
}

func (a *AuthProvider) GetType() string {
	return a.Type
}

func (a *AuthProvider) GetID() string {
	return a.ID
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
