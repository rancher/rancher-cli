package token

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// TestAuthProviderWithUserIDMustBeAnAuthConfigName asserts --auth-provider
// with --user-id is checked as an auth config name before the cache is read
// or any request is made, on the value as typed: a provider type, a value
// with a space, an upper-case one and one with a slash are VALIDATION with
// the text that names what a value looks like; a provider name passes the
// check and is matched exactly later.
func TestAuthProviderWithUserIDMustBeAnAuthConfigName(t *testing.T) {
	for _, value := range []string{"openLdapProvider", " corp-ldap", "corp-ldap ", "Corp-Ldap", "corp/ldap", "corp:ldap", "a_b"} {
		t.Run(value, func(t *testing.T) {
			srv := failingRancher(t)
			dir := t.TempDir()
			store := &countingStore{CredStore: config.NewFileStore(dir)}
			cmd := NewTokenCommand(dir, store, anchorFor(dir)).(runtime.ReadCommand)
			ops := map[string]int{}
			t.Cleanup(config.SetIdentityObserverForTest(func(op string) { ops[op]++ }))
			_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": srv.URL, "cluster": "c-1", "name": "prod", "user-id": "u-abc12", "auth-provider": value}}, ui.Discard())
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, clierr.CodeValidation, ce.Code)
			assert.Equal(t, "--auth-provider is not an auth config name (a lowercase name such as corp-ldap)", ce.Message)
			assert.Empty(t, store.cacheReads(), "no record is read for an invalid value")
			assert.Empty(t, ops, "and nothing else is read: no stamp, no config")
		})
	}
}

// TestAuthProviderWithoutUserIDIsNotChecked asserts the flag keeps its
// meaning of choosing a sign-in when there is no --user-id: a provider type
// and a provider name are accepted, and the key of the cache slot carries no
// auth config.
func TestAuthProviderWithoutUserIDIsNotChecked(t *testing.T) {
	x := newAuthConfigFixture(t, false, testrancher.Options{})
	_, _, err := x.callWith(authCall{cluster: "c-test", name: "n", authConfig: "openLdapProvider"})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.NotEqual(t, clierr.CodeValidation, ce.Code, "a provider type is no VALIDATION without --user-id: %s", ce.Message)

	y := newAuthConfigFixture(t, false, testrancher.Options{})
	_, owner, err := y.callWith(authCall{cluster: "c-test", name: "n", authConfig: "openldap", signIn: signInLines("openldap", "alice")})
	require.NoError(t, err)
	assert.Equal(t, "user-alice", owner, "a provider name signs in through the auth config of that provider")
	assert.True(t, y.cachedWith("c-test", "n", "", ""), "the slot has no auth config")
}

// TestUserIDSignInMatchesTheAuthConfigByItsExactNameOnly asserts the
// sign-in of a call with --user-id and an auth config accepts only an
// enabled auth config of that name: a provider type or provider name that
// no auth config is named by fails with NOT_FOUND before any prompt, naming
// the enabled auth configs, with and without a cluster and with token reuse
// off and on (the plugin's own sign-in), where the same value without
// --user-id matches by provider name.
func TestUserIDSignInMatchesTheAuthConfigByItsExactNameOnly(t *testing.T) {
	only := testrancher.Options{AuthConfigs: []testrancher.AuthConfig{{Name: "corp-ldap", Type: "openLdapProvider", Users: authConfigUsers("corp-ldap")}}}
	newOnly := func(t *testing.T) *ownerFixture {
		t.Helper()
		return newOwnerFixture(t, false, only)
	}

	t.Run("the exact name signs in through that auth config", func(t *testing.T) {
		x := newOnly(t)
		var bodies []string
		var mu sync.Mutex
		x.hook = func(r *http.Request) {
			if r.URL.Path == "/v1-public/login" {
				mu.Lock()
				bodies = append(bodies, loginBodyConfigName(r))
				mu.Unlock()
			}
		}
		_, owner, err := x.callWith(authCall{cluster: "c-test", userID: "user-alice", authConfig: "corp-ldap", signIn: signInLines("corp-ldap", "alice")})
		require.NoError(t, err)
		assert.Equal(t, "user-alice", owner)
		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, []string{"corp-ldap"}, bodies, "the login names the auth config")
		assert.True(t, x.cachedWith("c-test", "", "user-alice", "corp-ldap"))
	})
	for _, c := range []struct {
		value, cluster string
		reuse          bool
	}{
		{"openldap", "c-test", false}, {"openLdapProvider", "c-test", false},
		{"openldap", "c-test", true}, {"openLdapProvider", "c-test", true},
		{"openldap", "", false}, {"openldap", "", true},
	} {
		value := c.value
		t.Run(fmt.Sprintf("no auth config is named %s, cluster %q, token reuse %t", value, c.cluster, c.reuse), func(t *testing.T) {
			x := newOwnerFixture(t, c.reuse, only)
			notices := &bytes.Buffer{}
			_, _, err := x.callWith(authCall{cluster: c.cluster, userID: "user-alice", authConfig: value, signIn: signInLines("corp-ldap", "alice"), notices: notices})
			if value == "openLdapProvider" {
				ce, _ := refusal(t, err)
				assert.Equal(t, clierr.CodeValidation, ce.Code, "a provider type is no auth config name")
				return
			}
			ce, ok := errors.AsType[*clierr.Error](err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, clierr.CodeNotFound, ce.Code)
			assert.Equal(t, "--auth-provider openldap names no enabled auth config on this server", ce.Message)
			assert.NotEmpty(t, ce.Candidates, "the enabled auth configs are the candidates")
			names := []string{}
			for _, c := range ce.Candidates {
				names = append(names, c.ID)
			}
			assert.Contains(t, names, "corp-ldap")
			assert.NotContains(t, notices.String(), "Enter username", "no prompt is shown")
			for _, r := range x.seen() {
				assert.NotEqual(t, "/v1-public/login", r.path, "no sign-in is sent")
			}
		})
	}
	t.Run("the same value without --user-id matches by provider name", func(t *testing.T) {
		x := newOnly(t)
		_, owner, err := x.callWith(authCall{cluster: "c-test", name: "n", authConfig: "openldap", signIn: signInLines("corp-ldap", "alice")})
		require.NoError(t, err)
		assert.Equal(t, "user-alice", owner)
	})
	t.Run("an auth config this CLI cannot sign in with keeps its own refusal", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{ExtraProviders: []string{"githubProvider"}})
		_, _, err := x.callWith(authCall{cluster: "c-test", userID: "user-alice", authConfig: "github", signIn: []string{"alice", "pw-alice"}})
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code)
		assert.True(t, strings.HasPrefix(ce.Message, "--auth-provider github names an auth config this CLI cannot sign in with"), ce.Message)
	})
}

// loginBodyConfigName is the configName of a login request's body, which the
// request keeps readable for the facade.
func loginBodyConfigName(r *http.Request) string {
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body struct {
		ConfigName string `json:"configName"`
	}
	_ = json.Unmarshal(raw, &body)
	return body.ConfigName
}
