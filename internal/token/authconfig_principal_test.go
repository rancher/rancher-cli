package token

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTheAuthConfigOfAPrincipalIsItsPrefix asserts the auth config a
// principal names is the prefix of its name as Rancher reads it: the part
// before "_<type>://", "local" for local://<id>, and none for a name that
// does not parse that way or names an empty auth config - never the
// principal's provider, which can equal another auth config's name.
func TestTheAuthConfigOfAPrincipalIsItsPrefix(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"corp-ldap_user://u-abc12": "corp-ldap",
		"openldap_user://u-abc12":  "openldap",
		"github_org://12345":       "github",
		"local://u-abc12":          "local",
		"":                         "",
		"garbage":                  "",
		"garbage://x":              "",
		"_user://x":                "",
		"corp-ldap_user":           "",
		"user:x":                   "",
	} {
		assert.Equal(t, want, authConfigOfPrincipal(name), name)
	}
}

// TestACreateResponseNamesItsAuthConfigThroughItsPrincipal asserts the create
// results of the two token shapes: an ext create names the auth config its
// principal names, a v3 create, whose userPrincipal is a printed map and
// whose authProvider is a provider name, names none, and neither does an ext
// create that carries no principal.
func TestACreateResponseNamesItsAuthConfigThroughItsPrincipal(t *testing.T) {
	t.Parallel()
	created := func(kind TokenKind, body string) CreateResult {
		t.Helper()
		var probe map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(body), &probe))
		resp := &http.Response{StatusCode: http.StatusCreated}
		return classifyCreate(resp, []byte(body), nil, "2026-10-19T00:00:00Z", true, kind)
	}
	ext := created(TokenKindExt, `{"metadata":{"name":"token-a"},"spec":{"userID":"u-1","userPrincipal":{"name":"corp-ldap_user://u-1","provider":"openldap"}},"status":{"bearerToken":"ext/token-a:s"}}`)
	assert.Equal(t, "corp-ldap", ext.AuthConfig)
	assert.Equal(t, "u-1", ext.UserID)
	none := created(TokenKindExt, `{"metadata":{"name":"token-a"},"spec":{"userID":"u-1"},"status":{"bearerToken":"ext/token-a:s"}}`)
	assert.Empty(t, none.AuthConfig)
	v3 := created(TokenKindV3, `{"id":"token-b","token":"token-b:s","userId":"u-1","authProvider":"openldap","userPrincipal":"map[name:corp-ldap_user://u-1 provider:openldap]"}`)
	assert.Empty(t, v3.AuthConfig, "a v3 create records none: its authProvider is a provider name")
	assert.Equal(t, "u-1", v3.UserID)
}
