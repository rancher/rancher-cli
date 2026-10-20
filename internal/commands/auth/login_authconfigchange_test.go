package auth

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// authConfigChangeFixture is a Rancher with two LDAP auth configs, each with
// alice and bob under passwords of their own, and a directory with the server
// entry prod.
type authConfigChangeFixture struct {
	t     *testing.T
	dir   string
	store config.CredStore
	srv   *httptest.Server
	cmd   *loginCmd
}

func newAuthConfigChangeFixture(t *testing.T) *authConfigChangeFixture {
	t.Helper()
	users := func(config string) []testrancher.User {
		return []testrancher.User{
			{Name: "alice", Password: "pw-" + config, UserID: "user-alice"},
			{Name: "bob", Password: "pw-" + config, UserID: "user-bob"},
		}
	}
	f := testrancher.New(testrancher.Options{AuthConfigs: []testrancher.AuthConfig{
		{Name: "corp-ldap", Type: "openLdapProvider", Users: users("corp-ldap")},
		{Name: "openldap", Type: "openLdapProvider", Users: users("openldap")},
	}})
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	dir, store := cmdtest.FilePolicyDir(t)
	seedServer(t, dir, "prod", srv.URL, "", true)
	return &authConfigChangeFixture{t: t, dir: dir, store: store, srv: srv, cmd: newPlainLoginCmd(dir, store)}
}

// signIn logs in to prod through authConfig as user and returns the result's
// warnings.
func (x *authConfigChangeFixture) signIn(authConfig, user string) []string {
	x.t.Helper()
	in := cmdtest.Input{Str: map[string]string{"auth-provider": authConfig}, Pos: []string{"prod"}}
	res, err := x.cmd.Execute(x.t.Context(), in, ui.New(&bytes.Buffer{}, newLineReader(user, "pw-"+authConfig), false, true))
	require.NoError(x.t, err)
	return res.Warnings
}

// pasteToken logs in to prod with a token that is stored without asking the
// server about it, so the entry records no user and no auth config.
func (x *authConfigChangeFixture) pasteToken() []string {
	x.t.Helper()
	in := cmdtest.Input{Flag: map[string]bool{"token-stdin": true, "no-verify": true}, Pos: []string{"prod"}}
	res, err := x.cmd.Execute(x.t.Context(), in, cmdtest.StdinPrompter("id:secret"))
	require.NoError(x.t, err)
	return res.Warnings
}

func (x *authConfigChangeFixture) recorded() (user, authConfig string) {
	x.t.Helper()
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(x.t, err)
	return cfg.Servers["prod"].TokenUserID, cfg.Servers["prod"].TokenAuthConfig
}

// authConfigChange is the warnings that say which auth config the entry is
// logged in through now.
func authConfigChange(warnings []string) []string {
	var out []string
	for _, w := range warnings {
		if strings.HasPrefix(w, "server entry prod is now logged in through auth config") {
			out = append(out, w)
		}
	}
	return out
}

const authConfigChangeWarning = "server entry prod is now logged in through auth config openldap; it was logged in through auth config corp-ldap"

// TestALoginRecordsTheAuthConfigItsTokenWasMadeThrough asserts a sign-in
// records the auth config the token create named on the entry, along with
// its owner.
func TestALoginRecordsTheAuthConfigItsTokenWasMadeThrough(t *testing.T) {
	t.Parallel()
	x := newAuthConfigChangeFixture(t)
	x.signIn("corp-ldap", "alice")
	user, authConfig := x.recorded()
	assert.Equal(t, "user-alice", user)
	assert.Equal(t, "corp-ldap", authConfig)
}

// TestAPastedTokenRecordsNoAuthConfigAndClearsTheOneBefore asserts a login
// of a pasted token records no auth config, the entry's previous one included
// - the first token created from it records one - and gives no warning.
func TestAPastedTokenRecordsNoAuthConfigAndClearsTheOneBefore(t *testing.T) {
	t.Parallel()
	x := newAuthConfigChangeFixture(t)
	x.signIn("corp-ldap", "alice")
	assert.Empty(t, authConfigChange(x.pasteToken()), "the new login records none, so nothing is said")
	user, authConfig := x.recorded()
	assert.Empty(t, user)
	assert.Empty(t, authConfig, "the previous auth config is not kept for a token that may be another's")
}

// TestAReLoginThroughAnotherAuthConfigWarns asserts a login that keeps the
// user and changes the auth config says so, once, as the user-change warning
// does, and records the new one.
func TestAReLoginThroughAnotherAuthConfigWarns(t *testing.T) {
	t.Parallel()
	x := newAuthConfigChangeFixture(t)
	assert.Empty(t, authConfigChange(x.signIn("corp-ldap", "alice")), "a first login says nothing")
	assert.Equal(t, []string{authConfigChangeWarning}, authConfigChange(x.signIn("openldap", "alice")))
	_, authConfig := x.recorded()
	assert.Equal(t, "openldap", authConfig, "the login succeeds and records the new auth config")
	assert.Empty(t, authConfigChange(x.signIn("openldap", "alice")), "the same auth config says nothing")
}

// TestAReLoginThatChangesTheUserAndTheAuthConfigWarnsOfBoth asserts both
// changes are said, one line each.
func TestAReLoginThatChangesTheUserAndTheAuthConfigWarnsOfBoth(t *testing.T) {
	t.Parallel()
	x := newAuthConfigChangeFixture(t)
	x.signIn("corp-ldap", "alice")
	warnings := x.signIn("openldap", "bob")
	assert.Equal(t, []string{authConfigChangeWarning}, authConfigChange(warnings))
	var users []string
	for _, w := range warnings {
		if strings.HasPrefix(w, "server entry prod is now logged in as user") {
			users = append(users, w)
		}
	}
	assert.Equal(t, []string{userChangeWarning}, users)
}

// TestAReLoginWithAnUnrecordedAuthConfigSaysNothingOfIt asserts no warning
// when either login recorded none: an old login with none, a pasted token
// after one, and a login after a logout.
func TestAReLoginWithAnUnrecordedAuthConfigSaysNothingOfIt(t *testing.T) {
	t.Parallel()
	t.Run("the old login recorded none", func(t *testing.T) {
		t.Parallel()
		x := newAuthConfigChangeFixture(t)
		x.pasteToken()
		assert.Empty(t, authConfigChange(x.signIn("openldap", "alice")))
	})
	t.Run("after a logout", func(t *testing.T) {
		t.Parallel()
		x := newAuthConfigChangeFixture(t)
		x.signIn("corp-ldap", "alice")
		_, err := (&logoutCmd{dir: x.dir}).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
		require.NoError(t, err)
		_, authConfig := x.recorded()
		assert.Equal(t, "corp-ldap", authConfig, "a logout keeps the recorded auth config as it keeps the owner")
		assert.Empty(t, authConfigChange(x.signIn("openldap", "alice")), "no stored login is left to replace")
	})
}

// TestAFailedReLoginSaysNothingAboutTheAuthConfig asserts a re-login whose
// credential cannot be saved fails without saying the entry is logged in
// through the new auth config, and the entry keeps the one it recorded.
func TestAFailedReLoginSaysNothingAboutTheAuthConfig(t *testing.T) {
	t.Parallel()
	x := newAuthConfigChangeFixture(t)
	x.signIn("corp-ldap", "alice")
	failing := &loginCmd{dir: x.dir, newStore: func(string, string) (config.CredStore, string, error) {
		return cmdtest.SetErrStore{CredStore: x.store, SetErr: errors.New("keychain refused")}, "", nil
	}}
	in := cmdtest.Input{Str: map[string]string{"auth-provider": "openldap"}, Pos: []string{"prod"}}
	_, err := failing.Execute(t.Context(), in, ui.New(&bytes.Buffer{}, newLineReader("alice", "pw-openldap"), false, true))
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Empty(t, authConfigChange(ce.Warnings()))
	_, authConfig := x.recorded()
	assert.Equal(t, "corp-ldap", authConfig, "the failed login wrote the previous auth config back")
}
