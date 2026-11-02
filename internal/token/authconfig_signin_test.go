package token

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// loginResponseTypes records the responseType of every login the fixture
// Rancher receives from the moment it is called.
func (x *ownerFixture) loginResponseTypes() func() []string {
	var (
		mu    sync.Mutex
		types []string
	)
	x.hook = func(r *http.Request) {
		if r.URL.Path == "/v1-public/login" {
			mu.Lock()
			types = append(types, loginBodyResponseType(r))
			mu.Unlock()
		}
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), types...)
	}
}

// TestAnUnscopedUserIDSignInIsAClusterTokenCreateWithNoCluster asserts every
// call with --user-id and no --cluster signs in through the one lane the
// cluster's sign-in uses, with an empty cluster: the login asks for json,
// the ext create with the login token as bearer names no cluster, its
// response names the owner, so no owner request is made, and the token is
// cached in the unscoped slot of the user and the auth config.
func TestAnUnscopedUserIDSignInIsAClusterTokenCreateWithNoCluster(t *testing.T) {
	for _, tc := range []struct {
		name       string
		authConfig string
		lines      []string
	}{
		{"with an auth config", "corp-ldap", signInLines("corp-ldap", "alice")},
		{"without one", "", []string{"alice", "pw-alice"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newOwnerFixture(t, false, testrancher.Options{})
			if tc.authConfig != "" {
				x = newAuthConfigFixture(t, false, testrancher.Options{})
			}
			responseTypes := x.loginResponseTypes()
			_, owner, err := x.callWith(authCall{userID: "user-alice", authConfig: tc.authConfig, signIn: tc.lines})
			require.NoError(t, err)
			assert.Equal(t, "user-alice", owner)
			assert.Equal(t, []string{"json"}, responseTypes(), "the login asks for the json form, not a kubeconfig")
			for _, c := range x.f.CreateRequests() {
				assert.Empty(t, c.ClusterName, "no create names a cluster")
			}
			assert.Empty(t, x.ownerReviews(), "the create names the owner, so no owner request is made")
			assert.True(t, x.cachedWith("", "", "user-alice", tc.authConfig))
		})
	}
}

// TestAnOwnerRequestIsAskedThroughTheLocalClusterOnlyWhenTheCreateNamesNoOwner
// asserts an unscoped sign-in whose token create names no owner asks the
// owner request through the local cluster, and one whose create names the
// owner makes none.
func TestAnOwnerRequestIsAskedThroughTheLocalClusterOnlyWhenTheCreateNamesNoOwner(t *testing.T) {
	x := newOwnerFixture(t, false, testrancher.Options{Legacy: true, V3TokenCreate: true, V3CreateOmitOwner: true})
	_, owner, err := x.callWith(authCall{userID: "user-alice", signIn: []string{"alice", "pw-alice"}})
	require.NoError(t, err)
	assert.Equal(t, "user-alice", owner)
	assert.Equal(t, []string{"/k8s/clusters/local" + selfSubjectReviewPath}, x.ownerReviews())
}

// TestASignInThroughAnotherAuthConfigIsRefused asserts the token a sign-in
// creates is checked against the call's auth config: one made through
// another, which only a browser sign-in chooses for itself, and one whose
// response names none are revoked with their own bearer and fail the call,
// for a cluster and for none, and nothing is cached.
func TestASignInThroughAnotherAuthConfigIsRefused(t *testing.T) {
	for _, cluster := range []string{"c-test", ""} {
		name := "a cluster"
		if cluster == "" {
			name = "no cluster"
		}
		t.Run("another auth config, "+name, func(t *testing.T) {
			x := newAuthConfigFixture(t, false, testrancher.Options{PrincipalAuthConfig: "openldap"})
			_, _, err := x.callWith(authCall{cluster: cluster, userID: "user-alice", authConfig: "corp-ldap", signIn: signInLines("corp-ldap", "alice")})
			ce, msg := refusal(t, err)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			assert.True(t, strings.HasPrefix(msg, "the sign-in was made through auth config 'openldap', not 'corp-ldap', which this exec entry names with --auth-provider; "+
				"the token it created was revoked; on the sign-in page, choose auth config 'corp-ldap'"), msg)
			assert.GreaterOrEqual(t, x.revoked(), 1)
			assert.False(t, x.cachedWith(cluster, "", "user-alice", "corp-ldap"))
		})
		t.Run("no auth config named, "+name, func(t *testing.T) {
			x := newAuthConfigFixture(t, false, testrancher.Options{OmitPrincipalOnCreate: true})
			_, _, err := x.callWith(authCall{cluster: cluster, userID: "user-alice", authConfig: "corp-ldap", signIn: signInLines("corp-ldap", "alice")})
			ce, msg := refusal(t, err)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			assert.True(t, strings.HasPrefix(msg, "the token create response of the sign-in named no auth config; it cannot be checked against --auth-provider 'corp-ldap'; "+
				"the token it created was revoked"), msg)
			assert.GreaterOrEqual(t, x.revoked(), 1)
			assert.False(t, x.cachedWith(cluster, "", "user-alice", "corp-ldap"))
		})
	}
}

// TestTheUnscopedSignInsRefusalsNameNoCluster asserts the texts of a failed
// sign-in for a token for Rancher itself name no cluster and no remedy that
// corrects one: Rancher's refusal to create a token, and a Rancher that
// serves neither ext tokens nor a v3 create.
func TestTheUnscopedSignInsRefusalsNameNoCluster(t *testing.T) {
	t.Run("Rancher refuses the create with 403", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{DerivedCreateStatus: http.StatusForbidden})
		_, _, err := x.callWith(authCall{userID: "user-alice", signIn: []string{"alice", "pw-alice"}})
		ce, msg := refusal(t, err)
		assert.Contains(t, msg, "Rancher refused to create a token for this user")
		assert.NotContains(t, msg, "cluster", "no cluster is named")
		assert.Equal(t, clierr.CodeRBAC, ce.Code)
		details, ok := ce.Details.(clierr.LoginFailureDetails)
		require.True(t, ok, "%T", ce.Details)
		assert.Contains(t, details.Recovery, "an administrator can allow the user to create tokens")
		assert.NotContains(t, details.Recovery, "cluster", "no cluster is corrected")
	})
	t.Run("Rancher serves no ext tokens and no v3 create", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{Legacy: true})
		_, _, err := x.callWith(authCall{userID: "user-alice", signIn: []string{"alice", "pw-alice"}})
		_, msg := refusal(t, err)
		assert.Contains(t, msg, "the sign-in cannot create a token")
		assert.NotContains(t, msg, "cluster token")
	})
	t.Run("a cluster keeps its cluster text", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{Legacy: true})
		_, _, err := x.callWith(authCall{cluster: "c-test", userID: "user-alice", signIn: []string{"alice", "pw-alice"}})
		_, msg := refusal(t, err)
		assert.Contains(t, msg, "the sign-in cannot create a cluster token")
	})
}

// TestThePluginCreatedLoginIsNamedForTheServerTheUserAndTheAuthConfig
// asserts the plugin login entry the plugin creates for a call with
// --user-id and an auth config is named for the host, the user and the auth
// config, records both, and serves the next call; a call with a user and no
// auth config keeps the name with the user alone.
func TestThePluginCreatedLoginIsNamedForTheServerTheUserAndTheAuthConfig(t *testing.T) {
	x := newAuthConfigFixture(t, true, testrancher.Options{})
	_, owner, err := x.callWith(authCall{cluster: "c-test", userID: "user-alice", authConfig: "corp-ldap", signIn: signInLines("corp-ldap", "alice")})
	require.NoError(t, err)
	assert.Equal(t, "user-alice", owner)
	name := config.ExecLoginEntryNameFor(x.serverID(), "user-alice", "corp-ldap")
	assert.Equal(t, config.ExecLoginEntryName(x.serverID())+"-user-alice-corp-ldap", name, "the name carries the host, the user and the auth config")
	entry := x.entryNamed(name)
	assert.Equal(t, "user-alice", entry.TokenUserID)
	assert.Equal(t, "corp-ldap", entry.TokenAuthConfig)
	assert.Equal(t, clierr.ProvenanceExecLogin, entry.Provenance)
	x.forgetTraffic()
	_, owner, err = x.callWith(authCall{cluster: "local", userID: "user-alice", authConfig: "corp-ldap"})
	require.NoError(t, err)
	assert.Equal(t, "user-alice", owner, "the next call uses the entry with no terminal")
	assert.Equal(t, []string{name}, x.store.reads())

	y := newOwnerFixture(t, true, testrancher.Options{})
	_, _, err = y.callWith(authCall{cluster: "c-test", userID: "user-alice", signIn: []string{"alice", "pw-alice"}})
	require.NoError(t, err)
	assert.Equal(t, "user-alice", y.entryNamed(config.ExecLoginEntryNameFor(y.serverID(), "user-alice", "")).TokenUserID, "without an auth config the name has the user alone")
	assert.Equal(t, "local", y.entryNamed(config.ExecLoginEntryNameFor(y.serverID(), "user-alice", "")).TokenAuthConfig, "and records the auth config the response named, with or without one on the call")
}

// TestAPluginLoginEntryOfAnotherAuthConfigUnderTheNameIsNotWrittenOver
// asserts the plugin-created login does not write over the entry under its
// name that recorded another auth config: the call signs in for this call
// only, nothing is stored, and the entry is as it was.
func TestAPluginLoginEntryOfAnotherAuthConfigUnderTheNameIsNotWrittenOver(t *testing.T) {
	x := newAuthConfigFixture(t, true, testrancher.Options{})
	name := config.ExecLoginEntryNameFor(x.serverID(), "user-alice", "corp-ldap")
	x.entryThrough(name, "", "", "user-alice", "openldap", true)
	before := x.entryNamed(name)
	_, owner, err := x.callWith(authCall{cluster: "c-test", userID: "user-alice", authConfig: "corp-ldap", signIn: signInLines("corp-ldap", "alice")})
	require.NoError(t, err)
	assert.Equal(t, "user-alice", owner)
	assert.Equal(t, before, x.entryNamed(name), "the entry is as it was")
	login, _ := x.stored(name)
	assert.Empty(t, login, "no login was stored under it")
}

// TestTwoUsersAndAuthConfigsThatMakeOneEntryNameShareItSafely asserts the
// name is no key: the pairs (u-a, b) and (u, a-b) give one entry name, and
// the second call, finding the entry of another owner, signs in for this
// call only and leaves the first entry as it was.
func TestTwoUsersAndAuthConfigsThatMakeOneEntryNameShareItSafely(t *testing.T) {
	x := newOwnerFixture(t, true, testrancher.Options{AuthConfigs: []testrancher.AuthConfig{
		{Name: "a-b", Type: "openLdapProvider", Users: []testrancher.User{{Name: "zed", Password: "pw-zed", UserID: "user"}}},
	}})
	first := config.ExecLoginEntryNameFor(x.serverID(), "user-a", "b")
	second := config.ExecLoginEntryNameFor(x.serverID(), "user", "a-b")
	require.Equal(t, first, second, "the two pairs give one name")
	x.entryThrough(first, "", "", "user-a", "b", true)
	before := x.entryNamed(first)
	_, owner, err := x.callWith(authCall{cluster: "c-test", userID: "user", authConfig: "a-b", signIn: []string{"zed", "pw-zed"}})
	require.NoError(t, err, "the second pair signs in for this call")
	assert.Equal(t, "user", owner)
	assert.Equal(t, before, x.entryNamed(first), "and the first pair's entry is as it was")
	login, _ := x.stored(first)
	assert.Empty(t, login, "no login was stored under the shared name")
}

// TestAPluginCreatedLoginThroughAnotherAuthConfigStoresNothing asserts the
// plugin-created login's sign-in is checked against the call's auth config,
// in the two ways it can fail - made through another, or created tokens that
// name none - and in both nothing is stored and the tokens are revoked.
func TestAPluginCreatedLoginThroughAnotherAuthConfigStoresNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts testrancher.Options
		want string
	}{
		{"another auth config", testrancher.Options{PrincipalAuthConfig: "openldap"},
			"the sign-in was made through auth config 'openldap', not 'corp-ldap', which this exec entry names with --auth-provider; nothing was stored; on the sign-in page, choose auth config 'corp-ldap'"},
		{"no auth config", testrancher.Options{OmitPrincipalOnCreate: true},
			"the tokens the sign-in created named no auth config; they cannot be checked against --auth-provider 'corp-ldap'; nothing was stored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newAuthConfigFixture(t, true, tc.opts)
			_, _, err := x.callWith(authCall{cluster: "c-test", userID: "user-alice", authConfig: "corp-ldap", signIn: signInLines("corp-ldap", "alice")})
			ce, msg := refusal(t, err)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			assert.Contains(t, msg, tc.want)
			cfg, lerr := config.Load(config.FilePath(x.dir))
			require.NoError(t, lerr)
			assert.Empty(t, cfg.Servers, "no entry was written")
			assert.GreaterOrEqual(t, x.revoked(), 1)
		})
	}
}

// TestAPluginLoginEntryNameThatIsTooLongNamesTheAuthConfigAndASignIn asserts
// the length refusal's text for a call with an auth config - it names the
// user and the auth config, and offers a sign-in through it beside the
// piped token - and that it comes before the sign-in.
func TestAPluginLoginEntryNameThatIsTooLongNamesTheAuthConfigAndASignIn(t *testing.T) {
	x := newAuthConfigFixture(t, true, testrancher.Options{})
	user, authConfig := strings.Repeat("u", 250), strings.Repeat("a", 250)
	name := config.ExecLoginEntryNameFor(x.serverID(), user, authConfig)
	require.Greater(t, len(name), config.MaxEntryNameLength)
	notices := &bytes.Buffer{}
	_, _, err := x.callWith(authCall{cluster: "c-test", userID: user, authConfig: authConfig, signIn: []string{"alice", "pw"}, notices: notices})
	ce, msg := refusal(t, err)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
	url := clierr.ShellQuote(x.serverID())
	assert.Equal(t, "the exec plugin's login entry for this server, for Rancher user '"+user+"' through auth config '"+authConfig+"', would have a name of "+strconv.Itoa(len(name))+
		" characters, and the credential store accepts at most 467; <name> stands for a name of your choice; to log in under a shorter name, sign in with: rancher auth login <name> --url "+url+" --auth-provider '"+authConfig+
		"'; or pipe an API token of '"+user+"' made through auth config '"+authConfig+"' to: rancher auth login <name> --url "+url+" --token-stdin", msg)
	assert.NotContains(t, notices.String(), "Enter username", "no sign-in was started")
	for _, r := range x.seen() {
		assert.NotEqual(t, "/v1-public/login", r.path)
	}
}
