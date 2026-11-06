package testrancher

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
// scopes depends on the type: Azure AD has an array; the OIDC types have a
// string when the config sets scope and null when it does not, so their rows
// hold for a configured provider.
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
// redirectUrl for the type. For the OIDC types that is the form Rancher builds
// when the config has no rancherApiHost; with one, Rancher builds
// <rancherApiHost>/v1-oidc/<name>, which the facade does not serve. SAML
// entries carry the URL their sign-in starts at, formed from the auth config's
// name. Mutations: drop the per-type fields from the ExtraProviders entries;
// serve scopes as an array for genericOIDCProvider; serve githubProvider's
// redirectUrl as a path of the facade; serve Azure AD's redirectUrl empty
// (each fails its rows).
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

// recordedEntries reads the entries of the list observation of a recorded
// golden from path.
func recordedEntries(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var golden struct {
		Observations []struct {
			Name   string `json:"name"`
			Status int    `json:"status"`
			Body   struct {
				Data []map[string]any `json:"data"`
			} `json:"body"`
		} `json:"observations"`
	}
	require.NoError(t, json.Unmarshal(raw, &golden))
	require.Len(t, golden.Observations, 1)
	require.Equal(t, "list", golden.Observations[0].Name)
	require.Equal(t, http.StatusOK, golden.Observations[0].Status)
	return golden.Observations[0].Body.Data
}

// azureEntryForm asserts an Azure AD list entry has the form Rancher builds:
// redirectUrl is the authUrl, the client id and the Rancher URL in that
// order, with the response type and scope Rancher adds; the three endpoints
// are absolute https URLs ending in the name of their endpoint; and the
// tenant and client id are set. It returns the Rancher URL inside redirectUrl.
func azureEntryForm(t *testing.T, who string, entry map[string]any) string {
	t.Helper()
	field := func(key string) string {
		v, _ := entry[key].(string)
		assert.NotEmpty(t, v, "%s: %s", who, key)
		return v
	}
	authURL, clientID, redirect := field("authUrl"), field("clientId"), field("redirectUrl")
	field("tenantId")
	const tail = "&response_type=code&scope=openid"
	head := authURL + "?client_id=" + clientID + "&redirect_uri="
	if !assert.True(t, strings.HasPrefix(redirect, head) && strings.HasSuffix(redirect, tail), "%s: redirectUrl %q", who, redirect) {
		return ""
	}
	rancherURL := strings.TrimSuffix(strings.TrimPrefix(redirect, head), tail)
	assert.NotEmpty(t, rancherURL, who)
	for key, endpoint := range map[string]string{"authUrl": "/authorize", "tokenUrl": "/token", "deviceAuthUrl": "/devicecode"} {
		u, err := url.Parse(field(key))
		require.NoError(t, err, "%s: %s", who, key)
		assert.Equal(t, "https", u.Scheme, "%s: %s", who, key)
		assert.NotEmpty(t, u.Host, "%s: %s", who, key)
		assert.True(t, strings.HasSuffix(u.Path, endpoint), "%s: %s %q", who, key, u.Path)
	}
	return rancherURL
}

// checkProviderEntries asserts the facade's list entries against the list a
// real server recorded at path: local and the three configs the probe
// enables, sorted by id (the probe sorts the list; Rancher's order is not
// fixed); for each recorded entry the keys are those of its provider type
// plus the three logout booleans, and the facade, set up with an
// auth config of the same name and type, lists the same keys without the
// booleans, with the same kind of value under each, the Okta redirectUrl
// ending in /v1-saml/okta/login on the facade's origin, and the Azure AD
// scopes and the form of its redirectUrl and endpoints equal.
func checkProviderEntries(t *testing.T, path string) {
	t.Helper()
	entries := recordedEntries(t, path)
	var ids []string
	for _, e := range entries {
		id, _ := e["id"].(string)
		ids = append(ids, id)
	}
	require.Equal(t, []string{"azuread", "local", "okta", "openldap"}, ids, "local and the three configs, sorted by id")

	for _, recorded := range entries {
		id, _ := recorded["id"].(string)
		typ, _ := recorded["type"].(string)
		keysOfType, known := rancherEntryKeys[typ]
		require.True(t, known, "the recorded entry %s has a type with no row: %s", id, typ)
		wantKeys := slices.Sorted(slices.Values(slices.Concat([]string{"id", "type"}, slices.Collect(maps.Keys(keysOfType)), absentOnPurpose)))
		assert.Equal(t, wantKeys, slices.Sorted(maps.Keys(recorded)), "the recorded keys of %s", id)
		for _, k := range absentOnPurpose {
			assert.IsType(t, true, recorded[k], "%s: %s", id, k)
		}

		var opts Options
		if id != "local" {
			opts.AuthConfigs = []AuthConfig{{Name: id, Type: typ}}
		}
		srv := httptest.NewServer(New(opts))
		t.Cleanup(srv.Close)
		origin := "https://" + srv.Listener.Addr().String()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1-public/authproviders", nil)
		require.NoError(t, err)
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var list struct {
			Data []map[string]any `json:"data"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
		_ = resp.Body.Close()
		var entry map[string]any
		for _, e := range list.Data {
			if e["id"] == id {
				entry = e
			}
		}
		require.NotNil(t, entry, "the facade lists %s", id)

		recordedKeys := slices.DeleteFunc(slices.Sorted(maps.Keys(recorded)), func(k string) bool { return slices.Contains(absentOnPurpose, k) })
		assert.Equal(t, recordedKeys, slices.Sorted(maps.Keys(entry)), "the facade's keys of %s", id)
		for key, value := range entry {
			assert.Equal(t, kindOf(recorded[key]), kindOf(value), "%s: the kind of %s", id, key)
		}

		switch typ {
		case "oktaProvider":
			const suffix = "/v1-saml/okta/login"
			redirect, _ := recorded["redirectUrl"].(string)
			assert.True(t, strings.HasSuffix(redirect, suffix), "the recorded redirectUrl %q", redirect)
			assert.Equal(t, origin+suffix, entry["redirectUrl"])
		case "azureADProvider":
			assert.Equal(t, recorded["scopes"], entry["scopes"])
			recordedRancherURL := azureEntryForm(t, "recorded", recorded)
			assert.Equal(t, "https://e2e-fid-rancher.invalid", recordedRancherURL, "the recorded redirect_uri is the rancherUrl the probe sets")
			assert.Equal(t, origin, azureEntryForm(t, "facade", entry))
		}
	}
}

// writeCannedExternalProviders writes a list in the shape of a recorded
// golden, holding the entries Rancher v2.15 builds for the values the probe
// sets. It is a fixture the test writes, not a recording.
func writeCannedExternalProviders(t *testing.T) string {
	t.Helper()
	entry := func(id, typ string, logoutSupported bool, extra map[string]any) map[string]any {
		e := map[string]any{"id": id, "type": typ, "logoutAllSupported": logoutSupported, "logoutAllEnabled": false, "logoutAllForced": false}
		maps.Copy(e, extra)
		return e
	}
	const authURL = "https://e2e-fid-azure.invalid/e2e-fid-tenant/oauth2/v2.0/authorize"
	data := []any{
		entry("azuread", "azureADProvider", true, map[string]any{
			"redirectUrl":   authURL + "?client_id=e2e-fid-app&redirect_uri=https://e2e-fid-rancher.invalid&response_type=code&scope=openid",
			"tenantId":      "e2e-fid-tenant",
			"clientId":      "e2e-fid-app",
			"scopes":        []any{"openid", "profile", "email"},
			"authUrl":       authURL,
			"tokenUrl":      "https://e2e-fid-azure.invalid/e2e-fid-tenant/oauth2/v2.0/token",
			"deviceAuthUrl": "https://e2e-fid-azure.invalid/e2e-fid-tenant/oauth2/v2.0/devicecode",
		}),
		entry("local", "localProvider", false, nil),
		entry("okta", "oktaProvider", true, map[string]any{"redirectUrl": "https://e2e-fid-okta.invalid/v1-saml/okta/login"}),
		entry("openldap", "openLdapProvider", false, nil),
	}
	golden := map[string]any{
		"oracleImage": "rancher/rancher:v2.15.2",
		"observations": []any{map[string]any{
			"name": "list", "status": http.StatusOK, "contentType": "application/json",
			"body": map[string]any{"data": data},
		}},
	}
	raw, err := json.MarshalIndent(golden, "", "  ")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "external-providers-enabled.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

// TestFacadeEntriesMatchAListInTheShapeOfARecordedOne asserts the checks that
// compare the facade's entries with the recorded external providers list pass
// on a list in the shape of a recorded golden, written by the test.
func TestFacadeEntriesMatchAListInTheShapeOfARecordedOne(t *testing.T) {
	t.Parallel()
	checkProviderEntries(t, writeCannedExternalProviders(t))
}

// TestFacadeEntriesMatchTheRecordedExternalProviders asserts the facade's
// entries match the list the oracle recorded with the three external
// providers enabled, through checkProviderEntries.
func TestFacadeEntriesMatchTheRecordedExternalProviders(t *testing.T) {
	t.Parallel()
	checkProviderEntries(t, filepath.Join("fidelity", "testdata", "oracle", "authproviders", "external-providers-enabled.json"))
}

// TestFacadeLocalEntryDiffersFromTheRecordedOneOnlyInTheLogoutBooleans
// asserts the entries of the facade's recorded list, with local alone enabled,
// equal the oracle's recorded entries once the three logout booleans are
// removed from each of the oracle's, and that every oracle entry has all
// three.
func TestFacadeLocalEntryDiffersFromTheRecordedOneOnlyInTheLogoutBooleans(t *testing.T) {
	t.Parallel()
	oracle := recordedEntries(t, filepath.Join("fidelity", "testdata", "oracle", "authproviders", "only-local-enabled.json"))
	facade := recordedEntries(t, filepath.Join("fidelity", "testdata", "facade", "authproviders", "only-local-enabled.json"))
	require.Len(t, oracle, len(facade))
	require.NotEmpty(t, oracle)
	for i, entry := range oracle {
		for _, k := range absentOnPurpose {
			assert.IsType(t, false, entry[k], "oracle entry %d: %s", i, k)
			delete(entry, k)
		}
		assert.Equal(t, entry, facade[i], "entry %d", i)
	}
}
