package token

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// TestTheCreateKubeconfigRemedyNeedsALoginThroughTheAuthConfig asserts the
// no-terminal refusal offers create kubeconfig, which uses the current
// server's login without the plugin's check of the token's auth config, only
// when that login recorded the call's auth config: a login that recorded none
// or another does not serve a call with one, and a call without one is served
// by any login of its user.
func TestTheCreateKubeconfigRemedyNeedsALoginThroughTheAuthConfig(t *testing.T) {
	const url = "https://r.example.com"
	for _, tc := range []struct {
		name       string
		recorded   string
		authConfig string
		userID     string
		serves     bool
	}{
		{"a login that recorded the auth config", "corp-ldap", "corp-ldap", "u-abc12", true},
		{"a login that recorded none", "", "corp-ldap", "u-abc12", false},
		{"a login that recorded another", "openldap", "corp-ldap", "u-abc12", false},
		{"no auth config on the call", "openldap", "", "u-abc12", true},
		{"another user's login", "corp-ldap", "corp-ldap", "u-other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{
				"prod": {URL: url, TokenUserID: "u-abc12", TokenAuthConfig: tc.recorded},
			}}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())
			store := config.NewFileStore(dir)
			require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: url}, "id:secret"))
			cmd := tokenCmd{dir: dir, store: store}
			got := cmd.currentLoginServes(cfg, noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: url, userID: tc.userID, authConfig: tc.authConfig})
			assert.Equal(t, tc.serves, got)
			err := cmd.withNoTerminalRemedies(interactiveLoginRequired("local", false), noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: url, userID: tc.userID, authConfig: tc.authConfig})
			ce, msg := refusal(t, err)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			if tc.serves {
				assert.Contains(t, msg, "for a kubeconfig that carries its own token, run: rancher create kubeconfig --clusters 'c-1' --apply --yes")
			} else {
				assert.NotContains(t, msg, "create kubeconfig")
			}
		})
	}
}

// TestACallWithAnAuthConfigWithoutATerminalNamesIt asserts the no-terminal
// refusal of a call with --user-id and an auth config asks for a login
// made through that auth config, piped or signed in, and names the
// kubectl run that signs in through it.
func TestACallWithAnAuthConfigWithoutATerminalNamesIt(t *testing.T) {
	x := newAuthConfigFixture(t, false, testrancher.Options{})
	_, _, err := x.callWith(authCall{cluster: "c-test", name: "n", userID: "user-alice", authConfig: "corp-ldap"})
	ce, msg := refusal(t, err)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	assert.Contains(t, msg, "pipe an API token of Rancher user 'user-alice' made through auth config 'corp-ldap' to: rancher auth login 'n' --url ")
	assert.Contains(t, msg, "; or run kubectl once in a terminal and sign in as 'user-alice' through auth config 'corp-ldap', which lasts until the cached token expires")
}

// TestAStoredLoginThatRancherRefusesToCreateFromReportsRancherAndRecordsNothing
// asserts the rows of an auth config an administrator disabled or deleted: a
// stored login made through it fails at Rancher's create and the call reports
// Rancher's answer, never an auth config refusal, recording nothing; a sign-in
// through it fails before any prompt, naming the enabled auth configs, with
// token reuse off and on.
func TestAStoredLoginThatRancherRefusesToCreateFromReportsRancherAndRecordsNothing(t *testing.T) {
	x := newAuthConfigFixture(t, false, testrancher.Options{ScopedCreateStatus: http.StatusInternalServerError})
	x.entryThrough("prod", "corp-ldap", "alice", "user-alice", "", false)
	_, _, err := x.callWith(authCall{cluster: "c-test", name: "prod-c-test", userID: "user-alice", authConfig: "corp-ldap"})
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "%v", err)
	assert.NotContains(t, ce.Message, "auth config", "Rancher's refusal is reported, not an auth config refusal")
	assert.Empty(t, x.entryNamed("prod").TokenAuthConfig, "nothing is recorded")

	for _, reuse := range []bool{false, true} {
		y := newAuthConfigFixture(t, reuse, testrancher.Options{})
		_, _, err = y.callWith(authCall{cluster: "c-test", userID: "user-alice", authConfig: "gone-ldap", signIn: signInLines("corp-ldap", "alice")})
		ce, ok = errors.AsType[*clierr.Error](err)
		require.True(t, ok, "token reuse %t: %v", reuse, err)
		assert.Equal(t, clierr.CodeNotFound, ce.Code, "token reuse %t", reuse)
		assert.Equal(t, "--auth-provider gone-ldap names no enabled auth config on this server", ce.Message, "token reuse %t", reuse)
		for _, r := range y.seen() {
			assert.NotEqual(t, "/v1-public/login", r.path, "token reuse %t: no sign-in is sent", reuse)
		}
	}
}
