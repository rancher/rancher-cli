package token

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// callWithCA is call with --cacert ca and no terminal.
func (x *ownerFixture) callWithCA(name, userID, ca string) error {
	x.t.Helper()
	s := map[string]string{"server": x.srv.URL, "cluster": "c-test", "name": name, "user-id": userID, "cacert": ca}
	cmd := NewTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
	_, err := cmd.Execute(x.t.Context(), tin{s: s}, ui.New(&bytes.Buffer{}, strings.NewReader(""), false, false))
	return err
}

// TestUserIDNewEntryLoginsCarryTheCallsCA asserts the CA the call resolved
// reaches the login of a new entry where each refusal that names one is
// raised: the refusal of a plugin login entry recording another owner, and
// the refusal of a token created from one that another owner's login
// made. A CA given as a path is named with --cacert; one given inline is
// asked for as a file, since the command cannot name it.
func TestUserIDNewEntryLoginsCarryTheCallsCA(t *testing.T) {
	pemCA := selfSignedCAPEM(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, []byte(pemCA), 0o600))
	for _, tc := range []struct {
		name, ca string
	}{
		{"a CA path", caPath},
		{"an inline CA", pemCA},
	} {
		wantCA := func(t *testing.T, x *ownerFixture, msg string) {
			t.Helper()
			if tc.ca == caPath {
				assert.Contains(t, msg, "rancher auth login <name> --url '"+x.serverID()+"' --cacert '"+caPath+"' --token-stdin")
				return
			}
			assert.Contains(t, msg, "this exec entry carries the CA inline and --cacert needs a file; ")
			assert.Contains(t, msg, "rancher auth login <name> --url '"+x.serverID()+"' --cacert <file> --token-stdin")
		}
		t.Run("another owner's plugin login entry, "+tc.name, func(t *testing.T) {
			x := newOwnerFixture(t, true, testrancher.Options{})
			x.entry(x.pluginEntry(), "bob", "user-bob", true)
			_, msg := refusal(t, x.callWithCA(x.pluginEntry()+"-c-test", "user-alice", tc.ca))
			assert.True(t, strings.HasPrefix(msg, "server entry '"+x.pluginEntry()+"', the exec plugin's own login entry, holds the login of Rancher user 'user-bob'"), msg)
			wantCA(t, x, msg)
		})
		t.Run("a token created from another owner's plugin login entry, "+tc.name, func(t *testing.T) {
			x := newOwnerFixture(t, true, testrancher.Options{})
			x.entry(x.pluginEntry(), "bob", "", true)
			_, msg := refusal(t, x.callWithCA(x.pluginEntry()+"-c-test", "user-alice", tc.ca))
			assert.True(t, strings.HasPrefix(msg, "the stored login of server entry '"+x.pluginEntry()+"' belongs to Rancher user 'user-bob'"), msg)
			wantCA(t, x, msg)
		})
	}
}

// TestUserIDPluginCreatedLoginChecksTheSuffixedEntryItWrites asserts the
// owner check of the entry the plugin-created login would write is made
// on the entry the collision rule resolves: with the call's plugin login
// entry name taken by an entry at another URL, the URL's own is the
// suffixed one, and when that one records another owner the call signs in
// interactively and leaves both untouched.
func TestUserIDPluginCreatedLoginChecksTheSuffixedEntryItWrites(t *testing.T) {
	x := newOwnerFixture(t, true, testrancher.Options{})
	taken, suffixed := x.pluginEntryOf("user-carol"), x.pluginEntryOf("user-carol")+"-2"
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(t, err)
	cfg.Servers[taken] = config.Server{URL: "https://elsewhere.example.com", Provenance: clierr.ProvenanceExecLogin}
	cfg.Servers[suffixed] = config.Server{URL: x.srv.URL, Provenance: clierr.ProvenanceExecLogin, TokenUserID: "user-bob"}
	require.NoError(t, cfg.Write())
	require.Equal(t, suffixed, config.CollisionFreeName(cfg, taken, x.serverID()), "the URL's own plugin login entry is the suffixed one")

	var notices bytes.Buffer
	got, err := x.call("kube-c-test", "user-carol", "carol", &notices)
	require.NoError(t, err)
	assert.Equal(t, "user-carol", got, "the call signs in interactively")
	login, s := x.stored(suffixed)
	assert.Empty(t, login, "the other user's entry is not logged in")
	assert.Equal(t, "user-bob", s.TokenUserID)
	after, err := config.Load(config.FilePath(x.dir))
	require.NoError(t, err)
	assert.Equal(t, cfg.Servers[taken], after.Servers[taken], "the entry at the other URL is untouched")
	assert.NotContains(t, notices.String(), "plaintext file", "no disclosure for a login that does not happen")
}

// TestUserIDPluginLaneWithoutATerminalNamesTheUser asserts the refusal a
// call with --user-id gets without a terminal on its way to the
// plugin-created login - token reuse on, its entry with no login, no
// plugin login entry yet: the login it names is a token of the call's
// user, and the sign-in in a terminal is as that user and is kept.
func TestUserIDPluginLaneWithoutATerminalNamesTheUser(t *testing.T) {
	x := newOwnerFixture(t, true, testrancher.Options{})
	x.entry("prod", "", "", false)
	_, err := x.call("prod-c-test", "user-carol", "", nil)
	ce, msg := refusal(t, err)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	assert.Equal(t, "interactive login required for auth config local; this call has no terminal to answer it; to log in without one, pipe an API token of Rancher user 'user-carol' to: "+
		"rancher auth login 'prod' --url '"+x.serverID()+"' --token-stdin; or run kubectl once in a terminal and sign in as 'user-carol'", msg)
	_, plugin := x.stored(x.pluginEntry())
	assert.Empty(t, plugin.URL, "nothing is written")
}

// TestCreateKubeconfigIsOfferedOnlyWhenTheCurrentLoginServes asserts the
// refusal without a terminal names create kubeconfig, which uses the
// directory's current server and its login, only when that login would
// give the call a kubeconfig for its server and user: the current server
// answers for the call's URL and holds a login, which, for a call with
// --user-id, records that user.
func TestCreateKubeconfigIsOfferedOnlyWhenTheCurrentLoginServes(t *testing.T) {
	const url = "https://r.example.com"
	for _, tc := range []struct {
		name    string
		current config.Server
		login   bool
		userID  string
		offered bool
	}{
		{"a login at the call's server", config.Server{URL: url}, true, "", true},
		{"a login recording the call's user", config.Server{URL: url, TokenUserID: "u-abc12"}, true, "u-abc12", true},
		{"a login recording another user", config.Server{URL: url, TokenUserID: "u-other"}, true, "u-abc12", false},
		{"a login recording no user", config.Server{URL: url}, true, "u-abc12", false},
		{"a login at another server", config.Server{URL: "https://other.example.com"}, true, "", false},
		{"no login", config.Server{URL: url}, false, "", false},
		{"a blank login", config.Server{URL: url}, true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": tc.current}}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())
			store := config.NewFileStore(dir)
			if tc.login {
				tok := "id:secret"
				if tc.name == "a blank login" {
					tok = "  "
				}
				require.NoError(t, store.SetToken(config.ServerRef{Name: "prod", URL: tc.current.URL}, tok))
			}
			err := tokenCmd{dir: dir, store: store}.withNoTerminalRemedies(interactiveLoginRequired("localProvider", false),
				noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: url, userID: tc.userID})
			_, msg := refusal(t, err)
			const remedy = "; for a kubeconfig that carries its own token, run: rancher create kubeconfig --clusters 'c-1' --apply --yes"
			if tc.offered {
				assert.Contains(t, msg, remedy)
				return
			}
			assert.NotContains(t, msg, "create kubeconfig")
		})
	}
	t.Run("a config.yaml that cannot be read", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(config.FilePath(dir), []byte("not: [yaml"), 0o600))
		err := tokenCmd{dir: dir, store: config.NewFileStore(dir)}.withNoTerminalRemedies(interactiveLoginRequired("localProvider", false),
			noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: url})
		_, msg := refusal(t, err)
		assert.NotContains(t, msg, "create kubeconfig")
	})
	t.Run("no store to read", func(t *testing.T) {
		dir := t.TempDir()
		cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{"prod": {URL: url}}}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		err := tokenCmd{dir: dir}.withNoTerminalRemedies(interactiveLoginRequired("localProvider", false),
			noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: url})
		_, msg := refusal(t, err)
		assert.NotContains(t, msg, "create kubeconfig")
	})
	t.Run("no current server", func(t *testing.T) {
		dir := t.TempDir()
		err := tokenCmd{dir: dir, store: config.NewFileStore(dir)}.withNoTerminalRemedies(interactiveLoginRequired("localProvider", false),
			noTerminalCall{name: "prod-c-1", clusterID: "c-1", serverURL: url})
		_, msg := refusal(t, err)
		assert.NotContains(t, msg, "create kubeconfig")
	})
}

// fateOf is the refused token's fate an owner refusal carries in its
// details, the shape an agent reads in data.error.details.
func fateOf(t *testing.T, err error) *clierr.TokenFate {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a typed error, got %v", err)
	d, ok := ce.Details.(directWaitDetails)
	require.True(t, ok, "the refusal carries the token's fate, got %T", ce.Details)
	require.NotNil(t, d.Fate)
	require.NotNil(t, d.Fate.Cleanup)
	return d.Fate
}

// signInDetails is the details of a refusal after an interactive sign-in: the
// login token's fate and the cluster token's, in the shape of a failed login.
func signInDetails(t *testing.T, err error) clierr.LoginFailureDetails {
	t.Helper()
	ce, ok := errors.AsType[*clierr.Error](err)
	require.True(t, ok, "want a typed error, got %v", err)
	d, ok := ce.Details.(clierr.LoginFailureDetails)
	require.True(t, ok, "the refusal carries both fates, got %T", ce.Details)
	require.NotNil(t, d.SessionToken)
	require.NotNil(t, d.ClusterToken)
	require.NotNil(t, d.ClusterToken.Cleanup)
	return d
}

// TestUserIDOwnerRefusalsCarryTheTokensFate asserts the details of the
// refusals of a token another owner's login made: the token's ref and
// what its revoke did - revoked, or failed with the server's answer, which
// the message then turns into the command that revokes it - and, for the
// refusal on a Rancher without ext tokens, the server it checked.
func TestUserIDOwnerRefusalsCarryTheTokensFate(t *testing.T) {
	t.Run("revoked", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		x.entry("prod", "bob", "", false)
		_, err := x.call("prod-c-test", "user-alice", "", nil)
		f := fateOf(t, err)
		require.NotNil(t, f.Ref)
		assert.Equal(t, clierr.TokenKindExt, f.Ref.Kind)
		assert.NotEmpty(t, f.Ref.Name)
		assert.Equal(t, ResultRevoked, f.Cleanup.Result)
	})
	t.Run("the revoke fails", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{ClusterDeleteMode: "fail"})
		x.entry("prod", "bob", "", false)
		_, err := x.call("prod-c-test", "user-alice", "", nil)
		f := fateOf(t, err)
		require.NotNil(t, f.Ref)
		assert.Equal(t, ResultFailed, f.Cleanup.Result)
		assert.Equal(t, "500 Internal Server Error", f.Cleanup.Error)
		_, msg := refusal(t, err)
		assert.Contains(t, msg, "the token created from it could not be revoked (500 Internal Server Error); revoke it with: rancher delete token '"+f.Ref.Name+"' --apply --yes")
	})
	t.Run("the sign-in of another user", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		_, err := x.call("prod-c-test", "user-alice", "carol", nil)
		d := signInDetails(t, err)
		f := d.ClusterToken
		require.NotNil(t, f.Ref)
		assert.Equal(t, clierr.TokenKindExt, f.Ref.Kind, "the sign-in creates an ext cluster token")
		assert.Equal(t, ResultRevoked, f.Cleanup.Result, "the token deletes itself")
		assert.Equal(t, ResultRevoked, sessionCleanup(t, d), "the login token was deleted before the check")
		assert.Equal(t, clierr.StageClusterToken, d.Stage)
	})
	t.Run("the sign-in of another user, on a Rancher without ext tokens", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{Legacy: true, V3TokenCreate: true})
		_, err := x.call("prod-c-test", "user-alice", "carol", nil)
		f := signInDetails(t, err).ClusterToken
		require.NotNil(t, f.Ref)
		assert.Equal(t, clierr.TokenKindV3, f.Ref.Kind)
		assert.Equal(t, ResultFailed, f.Cleanup.Result, "Rancher refuses a cluster-scoped token on both requests")
		assert.Equal(t, "logout 401; delete 403 Forbidden", f.Cleanup.Error)
		_, msg := refusal(t, err)
		assert.Contains(t, msg, "could not be revoked (logout 401; delete 403 Forbidden); v3/"+f.Ref.Name+" expires at ")
		assert.Contains(t, msg, "; an administrator can delete it sooner with: rancher delete management-token '"+f.Ref.Name+"' --apply --yes --allow-deprecated")
	})
	t.Run("no ext tokens", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{Legacy: true})
		x.entry("prod", "alice", "user-alice", false)
		_, err := x.call("prod-c-test", "user-alice", "", nil)
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok)
		d, ok := ce.Details.(clierr.CapabilityUnsupportedDetails)
		require.True(t, ok, "got %T", ce.Details)
		assert.Equal(t, clierr.CapabilityUnsupportedDetails{Feature: "ext.cattle.io/v1 tokens", Server: x.serverID()}, d)
	})
}

// TestNoTerminalLoginForAPluginLoginEntryNameGoesOn asserts the login a
// call without a terminal is given when its --name stands for one of the
// exec plugin's own login entries: a new entry, made current - with the
// way back to the current server when there is one - and the kubeconfig
// for the call's cluster written again from it; no hint, since the remedy
// is three commands.
func TestNoTerminalLoginForAPluginLoginEntryNameGoesOn(t *testing.T) {
	t.Parallel()
	const url = "https://r.example.com"
	cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{
		"prod":            {URL: url},
		"exec-login-r.ex": {URL: url, Provenance: clierr.ProvenanceExecLogin},
	}}
	const switchBack = "; selecting an entry switches the directory's current server; to switch back, run: rancher config use-server 'prod'"
	for _, tc := range []struct {
		name, cluster, want string
	}{
		{"a cluster", "c-1", "rancher auth login <name> --url '" + url + "' --token-stdin; " +
			"then rancher config use-server <name>; then rancher kubeconfig --cluster 'c-1'" + switchBack},
		{"no cluster", "", "rancher auth login <name> --url '" + url + "' --token-stdin; " +
			"then rancher config use-server <name>; then rancher kubeconfig --cluster <id>" + switchBack},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := NoTerminalLogin(cfg, "exec-login-r.ex-"+tc.cluster, tc.cluster, url, "", config.CASource{}, false)
			if tc.cluster == "" {
				l = NoTerminalLogin(cfg, "exec-login-r.ex", tc.cluster, url, "", config.CASource{}, false)
			}
			assert.Equal(t, tc.want, clierr.ExpandRemedies(l.Text(), ""))
			assert.Empty(t, l.Hint())
		})
	}
	user := NoTerminalLogin(cfg, "prod-c-1", "c-1", url, "", config.CASource{}, false)
	assert.Equal(t, "rancher auth login 'prod' --url '"+url+"' --token-stdin", clierr.ExpandRemedies(user.Text(), ""), "a user entry's login is enough alone")
}
