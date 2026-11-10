package token

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// TestUserIDPluginLoginEntryTheNameIdentifiesAnswersWithItsOwnLogin
// asserts a plugin login entry the --name identifies, with token reuse on,
// that holds a login and records the call's user or none answers alone: its
// login makes the token, which is cached under the name and the user id.
func TestUserIDPluginLoginEntryTheNameIdentifiesAnswersWithItsOwnLogin(t *testing.T) {
	for _, recorded := range []string{"user-alice", ""} {
		t.Run("recording "+recorded, func(t *testing.T) {
			x := newOwnerFixture(t, true, testrancher.Options{})
			name := x.pluginEntryOf("user-alice")
			x.entry(name, "alice", recorded, true)
			x.entry("prod", "alice", "user-alice", false)
			x.forgetReads()
			got, err := x.call(name+"-c-test", "user-alice", "", nil)
			require.NoError(t, err)
			assert.Equal(t, "user-alice", got)
			assert.Equal(t, []string{name}, x.store.reads(), "the entry the name identifies is read and nothing else")
			assert.True(t, x.cached(name+"-c-test", "user-alice"))
		})
	}
}

// TestUserIDPluginLoginEntryTheNameIdentifiesWithoutATerminal asserts the
// refusal for want of a terminal of a call whose --name identifies a plugin
// login entry that holds no login: with token reuse on the plugin-created
// login would write that entry, with it off the call signs in; both name a
// login of the user and a sign-in as that user.
func TestUserIDPluginLoginEntryTheNameIdentifiesWithoutATerminal(t *testing.T) {
	for _, reuse := range []bool{true, false} {
		t.Run(map[bool]string{true: "token reuse on", false: "token reuse off"}[reuse], func(t *testing.T) {
			x := newOwnerFixture(t, reuse, testrancher.Options{})
			name := x.pluginEntryOf("user-alice")
			x.entry(name, "", "user-alice", true)
			_, err := x.call(name+"-c-test", "user-alice", "", nil)
			ce, msg := refusal(t, err)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			assert.Contains(t, msg, "this call has no terminal to answer it; <name> stands for a name of your choice; to log in without a terminal, pipe an API token of Rancher user 'user-alice' to: rancher auth login <name> --url '"+x.serverID()+"'")
			assert.Contains(t, msg, "or run kubectl once in a terminal and sign in as 'user-alice'")
			assert.NotContains(t, msg, "sets no --name")
		})
	}
}

// TestUserIDChoiceBeyondTheFirstEntry asserts the cases a call with
// --user-id and no --name reaches after the choice: entries that record
// the user and hold no login lead to a sign-in, and an entry that records
// the user beside another user's login, which a failed login leaves, makes
// the call fail with the mismatch, revoking the token, and caches nothing.
func TestUserIDChoiceBeyondTheFirstEntry(t *testing.T) {
	t.Run("entries that hold no login lead to a sign-in", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		x.entry("a-user", "", "user-alice", false)
		x.forgetReads()
		got, err := x.call("", "user-alice", "alice", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-alice", got)
		assert.Equal(t, []string{"a-user"}, x.store.reads())
		assert.True(t, x.cachedIn("c-test", "", "user-alice"))
	})
	t.Run("the chosen entry holds another user's login", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		x.entry("a-user", "bob", "user-alice", false)
		_, err := x.call("", "user-alice", "", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Equal(t, "the stored login of server entry 'a-user' belongs to Rancher user 'user-bob', not to 'user-alice', which this exec entry names with --user-id; "+
			"the token created from it was revoked; pipe an API token of 'user-alice' to: rancher auth login 'a-user' --token-stdin", msg)
		assert.Equal(t, "rancher auth login 'a-user' --token-stdin", clierr.ExpandRemedies(ce.Hint, ""))
		assert.Equal(t, 1, x.revoked(), "the created token is revoked")
		assert.False(t, x.cachedIn("c-test", "", "user-alice"))
	})
}

// TestUserIDOnARancherWithoutExtTokensStillUsesAPluginLoginEntry asserts a
// call with --user-id fails closed on a user entry when the Rancher serves
// no ext.cattle.io, and still works through a plugin login entry that
// records the user, whose v3 create names the owner.
func TestUserIDOnARancherWithoutExtTokensStillUsesAPluginLoginEntry(t *testing.T) {
	x := newOwnerFixture(t, true, testrancher.Options{Legacy: true, V3TokenCreate: true})
	x.entry("prod", "alice", "user-alice", false)
	_, err := x.call("prod-c-test", "user-alice", "", nil)
	ce, _ := refusal(t, err)
	assert.Equal(t, clierr.CodeCapabilityUnsupported, ce.Code, "a user entry's login is never handed over")

	y := newOwnerFixture(t, true, testrancher.Options{Legacy: true, V3TokenCreate: true})
	y.entry("exec-login-one", "alice", "user-alice", true)
	got, err := y.call("kube-c-test", "user-alice", "", nil)
	require.NoError(t, err)
	assert.Equal(t, "user-alice", got, "a plugin login entry's v3 create names the owner")
}

// TestUserIDWithoutAClusterWaitsForNoDirectEndpoint asserts a call with
// --user-id and no --cluster never probes the direct endpoint kubectl names,
// which a token for Rancher itself does not reach, while the same call with
// a cluster waits for it.
func TestUserIDWithoutAClusterWaitsForNoDirectEndpoint(t *testing.T) {
	x := newOwnerFixture(t, false, testrancher.Options{})
	x.entry("prod", "alice", "user-alice", false)
	d := newDirectEndpoint(t, 0, "review")
	t.Setenv(execInfoEnv, execInfoFor(t, d.srv.URL, pemEncode(d.srv.Certificate().Raw)))

	var notices bytes.Buffer
	_, err := x.callIn("", "prod", "user-alice", "", &notices)
	require.NoError(t, err)
	assert.Zero(t, d.probes.Load(), "no direct endpoint is probed")
	assert.NotContains(t, notices.String(), "direct endpoint")

	_, err = x.callIn("c-test", "prod", "user-alice", "", &notices)
	require.NoError(t, err)
	assert.Positive(t, d.probes.Load(), "a call with a cluster waits for the endpoint")
}
