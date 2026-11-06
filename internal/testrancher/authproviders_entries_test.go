package testrancher

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// kind is the JSON kind of an entry's value: a string, or an array of strings.
type kind string

const (
	str kind = "string"
	arr kind = "array"
)

// rancherEntryKeys are the keys of the entry Rancher lists for each provider
// type besides the id and the type, which every entry has, with the kind of
// each value. A type with no extra key is listed with the two. The kind of
// scopes depends on the type: Azure AD has an array, the OIDC types a string.
var rancherEntryKeys = map[string]map[string]kind{
	"localProvider":           nil,
	"openLdapProvider":        nil,
	"freeIpaProvider":         nil,
	"activeDirectoryProvider": {"defaultLoginDomain": str},
	"azureADProvider":         {"redirectUrl": str, "tenantId": str, "clientId": str, "scopes": arr, "authUrl": str, "tokenUrl": str, "deviceAuthUrl": str},
	"pingProvider":            {"redirectUrl": str},
	"adfsProvider":            {"redirectUrl": str},
	"keyCloakProvider":        {"redirectUrl": str},
	"oktaProvider":            {"redirectUrl": str},
	"shibbolethProvider":      {"redirectUrl": str},
	"genericSAMLProvider":     {"redirectUrl": str},
	"githubProvider":          {"redirectUrl": str},
	"githubAppProvider":       {"redirectUrl": str},
	"googleOAuthProvider":     {"redirectUrl": str},
	"oidcProvider":            {"redirectUrl": str},
	"cognitoProvider":         {"redirectUrl": str, "scopes": str},
	"keyCloakOIDCProvider":    {"redirectUrl": str, "scopes": str},
	"genericOIDCProvider":     {"redirectUrl": str, "scopes": str},
}

// kindOf is the kind of a decoded JSON value, "" for any other.
func kindOf(v any) kind {
	switch v.(type) {
	case string:
		return str
	case []any:
		return arr
	}
	return ""
}

// absentOnPurpose are the keys Rancher sends on every entry that the facade
// leaves out: the three logout booleans, which the CLI does not read and
// which no non-test source file may name.
var absentOnPurpose = []string{"logoutAllSupported", "logoutAllEnabled", "logoutAllForced"}

// TestEveryListedProviderTypeHasTheKeysRancherSendsForIt asserts the
// facade's list of auth providers gives each provider type it can list the
// key set Rancher's list entry has for that type, whichever option lists it
// (the default entries, ExtraProviders, AuthConfigs), with the three logout
// booleans as the only keys left out, and the kind of each value (scopes is
// an array for Azure AD and a string for the OIDC types) and the form of the
// redirectUrl for the type. SAML entries carry the URL their sign-in starts
// at, formed from the auth config's name. Mutations: drop the per-type fields
// from the ExtraProviders entries; serve scopes as an array for
// genericOIDCProvider; serve githubProvider's redirectUrl as a path of the
// facade; serve Azure AD's redirectUrl empty (each fails its rows).
func TestEveryListedProviderTypeHasTheKeysRancherSendsForIt(t *testing.T) {
	t.Parallel()
	extra := []string{"pingProvider", "adfsProvider", "keyCloakProvider", "shibbolethProvider", "genericSAMLProvider",
		"githubAppProvider", "googleOAuthProvider", "oidcProvider", "cognitoProvider", "keyCloakOIDCProvider", "genericOIDCProvider"}
	f := New(Options{EnableIdP: true, ExtraProviders: extra, AuthConfigs: []AuthConfig{
		{Name: "corp-ldap", Type: "openLdapProvider"},
		{Name: "corp-ipa", Type: "freeIpaProvider"},
		{Name: "corp-ad", Type: "activeDirectoryProvider"},
		{Name: "corp-azure", Type: "azureADProvider"},
		{Name: "corp-okta", Type: "oktaProvider"},
		{Name: "corp-gh", Type: "githubProvider"},
	}})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	resp, err := srv.Client().Get(srv.URL + "/v1-public/authproviders")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))

	seen := map[string]bool{}
	for _, entry := range list.Data {
		typ, _ := entry["type"].(string)
		want, known := rancherEntryKeys[typ]
		require.True(t, known, "the facade lists a type with no row: %v", entry["type"])
		seen[typ] = true
		wantKeys := slices.Sorted(slices.Values(append([]string{"id", "type"}, slices.Collect(maps.Keys(want))...)))
		assert.Equal(t, wantKeys, slices.Sorted(maps.Keys(entry)), "%s entry %v", typ, entry["id"])
		for key, k := range want {
			assert.Equal(t, k, kindOf(entry[key]), "%s entry %v: the kind of %s", typ, entry["id"], key)
		}
		for _, k := range absentOnPurpose {
			assert.NotContains(t, entry, k, typ)
		}
	}
	for typ := range rancherEntryKeys {
		assert.True(t, seen[typ], "no entry of type %s was listed", typ)
	}

	origin := "https://" + srv.Listener.Addr().String()
	for _, entry := range list.Data {
		id, typ := entry["id"].(string), entry["type"].(string)
		redirect, _ := entry["redirectUrl"].(string)
		switch typ {
		case "pingProvider", "adfsProvider", "keyCloakProvider", "oktaProvider", "shibbolethProvider", "genericSAMLProvider":
			assert.Equal(t, origin+"/v1-saml/"+id+"/login", redirect, typ)
		case "githubProvider", "githubAppProvider":
			assert.True(t, strings.HasPrefix(redirect, "https://github.com/login/oauth/authorize?client_id="), "%s: %s", typ, redirect)
		case "googleOAuthProvider":
			assert.Equal(t, "https://accounts.google.com/o/oauth2/auth?access_type=offline&client_id=facade-client&prompt=consent&response_type=code", redirect, typ)
		case "oidcProvider", "cognitoProvider", "keyCloakOIDCProvider", "genericOIDCProvider":
			assert.True(t, strings.HasPrefix(redirect, origin+"/idp/authorize?client_id="), "%s: %s", typ, redirect)
			assert.Contains(t, redirect, "&response_type=code&redirect_uri="+origin, typ)
		case "azureADProvider":
			authURL, _ := entry["authUrl"].(string)
			assert.NotEmpty(t, authURL)
			clientID, _ := entry["clientId"].(string)
			assert.NotEmpty(t, clientID)
			assert.Equal(t, authURL+"?client_id="+clientID+"&redirect_uri="+origin+"&response_type=code&scope=openid", redirect, typ)
			assert.Equal(t, []any{"openid", "profile", "email"}, entry["scopes"])
		}
		if scopes, ok := entry["scopes"].(string); ok {
			assert.NotEmpty(t, scopes, typ)
		}
	}
}

// TestExtraProvidersRefusesAzureAD asserts the option that lists provider
// types the facade does not serve refuses azureADProvider, whose sign-in the
// facade serves through endpoints of its own that an entry listed that way
// would lack.
func TestExtraProvidersRefusesAzureAD(t *testing.T) {
	t.Parallel()
	require.PanicsWithValue(t, "testrancher: ExtraProviders cannot list azureADProvider; list it in AuthConfigs", func() {
		New(Options{ExtraProviders: []string{"githubProvider", "azureADProvider"}})
	})
}

// TestSAMLProviderTypesReturnsACopy asserts a caller cannot change the types
// the facade lists as SAML providers through the slice it gets.
func TestSAMLProviderTypesReturnsACopy(t *testing.T) {
	t.Parallel()
	got := SAMLProviderTypes()
	want := slices.Clone(got)
	got[0] = "changed"
	assert.Equal(t, want, SAMLProviderTypes())
}
