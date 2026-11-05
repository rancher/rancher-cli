package token

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// newAuthConfigFixture is the owner fixture on a Rancher with two LDAP auth
// configs, corp-ldap and openldap, beside the local one: alice and bob sign
// in through each with a password of its own, as the same Rancher users
// (user-alice, user-bob) whose principals name the auth config.
func newAuthConfigFixture(t *testing.T, reuse bool, opts testrancher.Options) *ownerFixture {
	t.Helper()
	opts.AuthConfigs = []testrancher.AuthConfig{
		{Name: "corp-ldap", Type: "openLdapProvider", Users: authConfigUsers("corp-ldap")},
		{Name: "openldap", Type: "openLdapProvider", Users: authConfigUsers("openldap")},
	}
	return newOwnerFixture(t, reuse, opts)
}

// authConfigUsers are alice and bob as one auth config knows them.
func authConfigUsers(authConfig string) []testrancher.User {
	var users []testrancher.User
	for _, u := range []string{"alice", "bob"} {
		users = append(users, testrancher.User{Name: u, Password: "pw-" + authConfig + "-" + u, UserID: "user-" + u})
	}
	return users
}

// signInLines are the answers a sign-in of user through authConfig asks
// for: the user name and the password.
func signInLines(authConfig, user string) []string {
	return []string{user, "pw-" + authConfig + "-" + user}
}

// acquireThrough is a login credential of user made through authConfig.
func (x *ownerFixture) acquireThrough(authConfig, user string) string {
	x.t.Helper()
	cred, err := AcquireLoginCredential(x.t.Context(), x.srv.Client(),
		LoginRequest{CAFlag: "--cacert", Server: x.srv.URL, AuthProvider: authConfig, ResponseType: "json"}, 0,
		ui.New(&bytes.Buffer{}, newLineReader(signInLines(authConfig, user)...), false, true))
	require.NoError(x.t, err)
	return cred.Token
}

// entryThrough writes a server entry at the fixture's URL that records
// owner as the login's owner and recorded as its auth config, with the
// login of user made through madeThrough stored when madeThrough is set. The
// recorded auth config can differ from the one the login was made through,
// as an entry that recorded none does.
func (x *ownerFixture) entryThrough(name, madeThrough, user, owner, recorded string, plugin bool) {
	x.t.Helper()
	x.entry(name, "", owner, plugin)
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(x.t, err)
	s := cfg.Servers[name]
	s.TokenAuthConfig = recorded
	cfg.Servers[name] = s
	require.NoError(x.t, cfg.Write())
	if madeThrough != "" {
		require.NoError(x.t, x.store.SetToken(config.ServerRef{Name: name, URL: x.srv.URL}, x.acquireThrough(madeThrough, user)))
	}
	x.forgetTraffic()
}

// forgetTraffic drops the requests and the store reads seen so far.
func (x *ownerFixture) forgetTraffic() {
	x.mu.Lock()
	x.requests = nil
	x.mu.Unlock()
	x.store.mu.Lock()
	x.store.read = nil
	x.store.mu.Unlock()
}

// authCall is one call of the exec plugin: its flags, and the answers of the
// terminal a sign-in asks, none when it has no terminal.
type authCall struct {
	cluster, name, userID, authConfig string
	signIn                            []string
	notices                           *bytes.Buffer
	// noInput is --no-input, with no terminal.
	noInput bool
	// prompter, when set, is the call's prompter in place of those.
	prompter runtime.Prompter
	// cacert is the call's --cacert: a path, or an inline PEM.
	cacert string
}

// callWith runs the exec plugin for c against the fixture and returns its
// result's token and the owner the fixture knows it by.
func (x *ownerFixture) callWith(c authCall) (token, owner string, err error) {
	x.t.Helper()
	res, err := x.executeWith(c)
	if err != nil {
		return "", "", err
	}
	cred, ok := res.Data.(*execCredential)
	require.True(x.t, ok)
	owner, found := x.f.BearerOwner(cred.Status.Token)
	require.True(x.t, found, "the served token is one the server issued")
	return cred.Status.Token, owner, nil
}

// executeWith runs the exec plugin for c against the fixture and returns
// its result, which is what the plugin writes to stdout, and its error.
func (x *ownerFixture) executeWith(c authCall) (runtime.Result, error) {
	x.t.Helper()
	s := map[string]string{"server": x.srv.URL}
	for k, v := range map[string]string{"cluster": c.cluster, "name": c.name, "user-id": c.userID, "auth-provider": c.authConfig, "cacert": c.cacert} {
		if v != "" {
			s[k] = v
		}
	}
	notices := c.notices
	if notices == nil {
		notices = &bytes.Buffer{}
	}
	p := ui.New(notices, strings.NewReader(""), c.noInput, false)
	if len(c.signIn) > 0 {
		p = ui.New(notices, newLineReader(c.signIn...), false, true)
	}
	cmd := NewTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
	if c.prompter != nil {
		return cmd.Execute(x.t.Context(), tin{s: s}, c.prompter)
	}
	return cmd.Execute(x.t.Context(), tin{s: s}, p)
}

// cachedWith reports whether a record is cached under the slot of name,
// userID and authConfig for cluster.
func (x *ownerFixture) cachedWith(cluster, name, userID, authConfig string) bool {
	x.t.Helper()
	_, _, err := x.store.GetCachedCred(config.CacheKeyFor(name, x.serverID(), cluster, userID, authConfig), anchorFor(x.dir))
	return err == nil
}

// entryNamed is the entry name holds in config.yaml now.
func (x *ownerFixture) entryNamed(name string) config.Server {
	x.t.Helper()
	_, s := x.stored(name)
	return s
}
