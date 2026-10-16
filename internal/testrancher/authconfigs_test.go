package testrancher_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/testrancher"
)

// authConfigOptions is a server with several auth configs of one type, as
// Rancher v2.16 lists them: two OpenLDAP configs and two Azure AD configs
// beside the default Azure AD entry, which the options rename.
func authConfigOptions() testrancher.Options {
	return testrancher.Options{
		EnableIdP:   true,
		AzureADName: "default-azure",
		AuthConfigs: []testrancher.AuthConfig{
			{Name: "corp-ldap", Type: "openLdapProvider", Users: []testrancher.User{{Name: "alice", Password: "pw", UserID: "user-corp"}}},
			{Name: "eu-ldap", Type: "openLdapProvider", Users: []testrancher.User{{Name: "alice", Password: "pw", UserID: "user-eu"}}},
			{Name: "corp-ad", Type: "activeDirectoryProvider", Users: []testrancher.User{{Name: "alice", Password: "pw", UserID: "user-ad"}}},
			{Name: "ipa", Type: "freeIpaProvider", Users: []testrancher.User{{Name: "alice", Password: "pw", UserID: "user-ipa"}}},
			{Name: "corp-azure", Type: "azureADProvider", Users: []testrancher.User{{Name: "corp", Password: "x", UserID: "user-azure-corp"}}},
		},
	}
}

// TestAuthProvidersListsSeveralConfigsOfOneType asserts the provider list
// has one entry per config, with distinct ids and the shared type; the
// default Azure AD entry's id is the one the options set, and each Azure AD
// config names its own client and endpoints.
func TestAuthProvidersListsSeveralConfigsOfOneType(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, authConfigOptions(), &recordingHandler{})
	resp, body := doJSON(t, srv.Client(), http.MethodGet, srv.URL+"/v1-public/authproviders", nil, "")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var decoded struct {
		Data []struct {
			ID            string `json:"id"`
			Type          string `json:"type"`
			ClientID      string `json:"clientId"`
			DeviceAuthURL string `json:"deviceAuthUrl"`
			TokenURL      string `json:"tokenUrl"`
		}
	}
	require.NoError(t, json.Unmarshal(body, &decoded))
	type entry struct{ id, typ string }
	var got []entry
	byID := map[string]int{}
	for i, e := range decoded.Data {
		got = append(got, entry{e.ID, e.Type})
		byID[e.ID] = i
	}
	assert.Equal(t, []entry{
		{"local", "localProvider"}, {"default-azure", "azureADProvider"},
		{"corp-ldap", "openLdapProvider"}, {"eu-ldap", "openLdapProvider"},
		{"corp-ad", "activeDirectoryProvider"}, {"ipa", "freeIpaProvider"}, {"corp-azure", "azureADProvider"},
	}, got)

	def, corp := decoded.Data[byID["default-azure"]], decoded.Data[byID["corp-azure"]]
	assert.Equal(t, "facade-client", def.ClientID)
	assert.Equal(t, "https://"+srv.Listener.Addr().String()+"/idp/devicecode", def.DeviceAuthURL)
	assert.Equal(t, "facade-client-corp-azure", corp.ClientID)
	assert.Equal(t, "https://"+srv.Listener.Addr().String()+"/idp/corp-azure/devicecode", corp.DeviceAuthURL)
	assert.Equal(t, "https://"+srv.Listener.Addr().String()+"/idp/corp-azure/token", corp.TokenURL)
}

// TestPasswordLoginsNeedTheirConfigName asserts an LDAP, FreeIPA or Active
// Directory login signs in through the config it names with that config's
// users, and is refused without configName, with one that names no config
// of the type, and with another config's name, as Rancher v2.16 does when
// it cannot read the config the login names.
func TestPasswordLoginsNeedTheirConfigName(t *testing.T) {
	t.Parallel()
	f, srv := newFacade(t, authConfigOptions(), &recordingHandler{})
	login := func(body map[string]any) int {
		body["responseType"] = "json"
		return statusOfBody(t, srv.Client(), http.MethodPost, srv.URL+"/v1-public/login", "", body)
	}
	for _, tc := range []struct{ typ, config string }{
		{"openLdapProvider", "corp-ldap"}, {"openLdapProvider", "eu-ldap"},
		{"activeDirectoryProvider", "corp-ad"}, {"freeIpaProvider", "ipa"},
	} {
		assert.Equal(t, http.StatusCreated, login(map[string]any{"type": tc.typ, "configName": tc.config, "username": "alice", "password": "pw"}), tc.config)
		assert.Equal(t, http.StatusUnauthorized, login(map[string]any{"type": tc.typ, "configName": tc.config, "username": "alice", "password": "wrong"}), tc.config)
		assert.Equal(t, http.StatusUnauthorized, login(map[string]any{"type": tc.typ, "username": "alice", "password": "pw"}), "no configName: "+tc.config)
		assert.Equal(t, http.StatusUnauthorized, login(map[string]any{"type": tc.typ, "configName": "nope", "username": "alice", "password": "pw"}), "unknown config")
	}
	assert.Equal(t, http.StatusUnauthorized, login(map[string]any{"type": "openLdapProvider", "configName": "corp-ad", "username": "alice", "password": "pw"}),
		"the name of a config of another type")

	records := f.LoginRecords()
	require.Len(t, records, 17)
	assert.Equal(t, testrancher.LoginRecord{Type: "openLdapProvider", ConfigName: "corp-ldap", HasConfigName: true}, records[0])
	assert.Equal(t, testrancher.LoginRecord{Type: "openLdapProvider", ConfigName: "", HasConfigName: false}, records[2], "a login that omits the field is told from one that sends it empty")
}

// TestAzureADLoginNeedsItsConfigName asserts an Azure AD login names the
// config it signs in through, whose id is the one the options set for the
// default entry.
func TestAzureADLoginNeedsItsConfigName(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, authConfigOptions(), &recordingHandler{})
	for _, tc := range []struct {
		config string
		prefix string
		status int
	}{
		{"default-azure", "/idp", http.StatusCreated},
		{"azuread", "/idp", http.StatusUnauthorized},
		{"", "/idp", http.StatusUnauthorized},
		{"corp-azure", "/idp/corp-azure", http.StatusCreated},
	} {
		body := map[string]any{"type": "azureADProvider", "responseType": "json", "id_token": idpIDToken(t, srv, tc.prefix)}
		if tc.config != "" {
			body["configName"] = tc.config
		}
		assert.Equal(t, tc.status, statusOfBody(t, srv.Client(), http.MethodPost, srv.URL+"/v1-public/login", "", body), "config %q", tc.config)
	}
}

// idpIDToken drives the identity-provider stub of the Azure AD config whose
// endpoints sit under prefix and returns the id_token it issues.
func idpIDToken(t *testing.T, srv *httptest.Server, prefix string) string {
	t.Helper()
	resp, body := postForm(t, srv.Client(), srv.URL+prefix+"/devicecode", map[string]string{"client_id": "c"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var dc struct {
		DeviceCode string `json:"device_code"`
	}
	require.NoError(t, json.Unmarshal(body, &dc))
	for {
		tokResp, tokBody := postForm(t, srv.Client(), srv.URL+prefix+"/token", map[string]string{"device_code": dc.DeviceCode})
		require.Equal(t, http.StatusOK, tokResp.StatusCode)
		var tokenJSON struct {
			IDToken string `json:"id_token"`
		}
		require.NoError(t, json.Unmarshal(tokBody, &tokenJSON))
		if tokenJSON.IDToken != "" {
			return tokenJSON.IDToken
		}
	}
}

// TestIdPRequestsRecordTheConfigEndpointsAndClient asserts the stub answers
// at each Azure AD config's own paths and records the path and the client
// id of each device-authorization request.
func TestIdPRequestsRecordTheConfigEndpointsAndClient(t *testing.T) {
	t.Parallel()
	f, srv := newFacade(t, authConfigOptions(), &recordingHandler{})
	idpIDToken(t, srv, "/idp")
	resp, _ := postForm(t, srv.Client(), srv.URL+"/idp/corp-azure/devicecode", map[string]string{"client_id": "facade-client-corp-azure"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp, _ = postForm(t, srv.Client(), srv.URL+"/idp/other/devicecode", map[string]string{"client_id": "x"})
	assert.NotEqual(t, http.StatusOK, resp.StatusCode, "a path of no config is not the stub's")
	assert.Equal(t, []testrancher.IdPRequest{
		{Path: "/idp/devicecode", ClientID: "c"},
		{Path: "/idp/corp-azure/devicecode", ClientID: "facade-client-corp-azure"},
	}, f.IdPRequests())
}

// TestPrincipalsCarryTheConfigNameAndTheProviderName asserts a token of a
// sign-in through config C names the principal C_user://<id> with the
// provider name of C's type, as Rancher v2.16 does, so a sign-in through
// another config of the type is another user, and tokens made with that
// bearer carry the same principal.
func TestPrincipalsCarryTheConfigNameAndTheProviderName(t *testing.T) {
	t.Parallel()
	_, srv := newFacade(t, authConfigOptions(), &recordingHandler{})
	principal := func(bearer string) (name, provider any) {
		p := postJSON(t, srv.Client(), srv.URL+extTokensPath, bearer, map[string]any{"spec": map[string]any{}})["spec"].(map[string]any)["userPrincipal"].(map[string]any)
		return p["name"], p["provider"]
	}
	signIn := func(body map[string]any) string {
		body["responseType"] = "json"
		return postJSON(t, srv.Client(), srv.URL+"/v1-public/login", "", body)["token"].(string)
	}
	for _, tc := range []struct {
		body      func() map[string]any
		principal string
		provider  string
	}{
		{func() map[string]any {
			return map[string]any{"type": "openLdapProvider", "configName": "corp-ldap", "username": "alice", "password": "pw"}
		}, "corp-ldap_user://user-corp", "openldap"},
		{func() map[string]any {
			return map[string]any{"type": "openLdapProvider", "configName": "eu-ldap", "username": "alice", "password": "pw"}
		}, "eu-ldap_user://user-eu", "openldap"},
		{func() map[string]any {
			return map[string]any{"type": "activeDirectoryProvider", "configName": "corp-ad", "username": "alice", "password": "pw"}
		}, "corp-ad_user://user-ad", "activedirectory"},
		{func() map[string]any {
			return map[string]any{"type": "freeIpaProvider", "configName": "ipa", "username": "alice", "password": "pw"}
		}, "ipa_user://user-ipa", "freeipa"},
		{func() map[string]any {
			return map[string]any{"type": "azureADProvider", "configName": "default-azure", "id_token": idpIDToken(t, srv, "/idp")}
		}, "default-azure_user://user-e2e", "azuread"},
		{func() map[string]any {
			return map[string]any{"type": "azureADProvider", "configName": "corp-azure", "id_token": idpIDToken(t, srv, "/idp/corp-azure")}
		}, "corp-azure_user://user-azure-corp", "azuread"},
	} {
		name, provider := principal(signIn(tc.body()))
		assert.Equal(t, tc.principal, name)
		assert.Equal(t, tc.provider, provider)
	}
}
