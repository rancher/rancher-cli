package token

import (
	"bytes"
	"errors"
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

// failingRead is a store whose credential read of the entries in fail
// returns the error given for them.
type failingRead struct {
	config.CredStore
	fail map[string]error
}

func (s failingRead) GetToken(ref config.ServerRef) (string, error) {
	if err, ok := s.fail[ref.Name]; ok {
		return "", err
	}
	return s.CredStore.GetToken(ref)
}

// setCurrent makes entry the directory's current server.
func (x *ownerFixture) setCurrent(entry string) {
	x.t.Helper()
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(x.t, err)
	cfg.CurrentServer = entry
	require.NoError(x.t, cfg.Write())
}

// forgetReads clears what the fixture's store and server have seen.
func (x *ownerFixture) forgetReads() {
	x.store.mu.Lock()
	x.store.read = nil
	x.store.mu.Unlock()
	x.mu.Lock()
	x.requests = nil
	x.mu.Unlock()
}

// TestUserIDWithoutANameChoosesAmongTheEntriesRecordingTheUser asserts a
// call with --user-id and no --name reads the entries for its URL that
// record the user - user entries before plugin login entries, the current
// server first, then by name - and the first that holds a login answers,
// the rest unread; an entry that records no owner or another user is
// never read, and a plugin login entry only while token reuse is on.
func TestUserIDWithoutANameChoosesAmongTheEntriesRecordingTheUser(t *testing.T) {
	fixture := func(t *testing.T, reuse bool) *ownerFixture {
		t.Helper()
		x := newOwnerFixture(t, reuse, testrancher.Options{})
		x.entry("b-user", "alice", "user-alice", false)
		x.entry("a-user", "alice", "user-alice", false)
		x.entry("c-bob", "bob", "user-bob", false)
		x.entry("d-unowned", "alice", "", false)
		x.entry(x.pluginEntryOf("user-alice"), "alice", "user-alice", true)
		x.forgetReads()
		return x
	}
	t.Run("the current server first", func(t *testing.T) {
		x := fixture(t, true)
		x.setCurrent("b-user")
		got, err := x.call("", "user-alice", "", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-alice", got)
		assert.Equal(t, []string{"b-user"}, x.store.reads(), "the first login answers and no other is read")
		assert.True(t, x.cachedIn("c-test", "", "user-alice"), "the token is cached under the user")
	})
	t.Run("then by name", func(t *testing.T) {
		x := fixture(t, true)
		_, err := x.call("", "user-alice", "", nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"a-user"}, x.store.reads())
	})
	t.Run("user entries before plugin login entries, each in order", func(t *testing.T) {
		x := fixture(t, true)
		for _, e := range []string{"a-user", "b-user"} {
			cfg, err := config.Load(config.FilePath(x.dir))
			require.NoError(t, err)
			require.NoError(t, x.store.DeleteToken(config.ServerRef{Name: e, URL: cfg.Servers[e].URL}))
		}
		x.setCurrent("b-user")
		x.forgetReads()
		got, err := x.call("", "user-alice", "", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-alice", got)
		assert.Equal(t, []string{"b-user", "a-user", x.pluginEntryOf("user-alice")}, x.store.reads(),
			"the entries without a login are read in order, then the plugin login entry answers")
	})
	t.Run("a plugin login entry is no candidate with token reuse off", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		x.entry(x.pluginEntryOf("user-alice"), "alice", "user-alice", true)
		x.forgetReads()
		_, err := x.call("", "user-alice", "", nil)
		ce, _ := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Empty(t, x.store.reads(), "its login is not read")
	})
	t.Run("an entry that records no owner or another user is never read", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		x.entry("c-bob", "bob", "user-bob", false)
		x.entry("d-unowned", "alice", "", false)
		x.forgetReads()
		_, err := x.call("", "user-alice", "", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.True(t, strings.HasPrefix(msg, "interactive login required for auth config local; this call has no terminal to answer it"), msg)
		assert.Empty(t, x.store.reads())
		assert.Empty(t, x.cachedIn("c-test", "", "user-alice"))
	})
	t.Run("a failed read ends the lookup with the failure", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		x.entry("a-user", "alice", "user-alice", false)
		x.entry("b-user", "alice", "user-alice", false)
		x.entry("c-user", "alice", "user-alice", false)
		x.setCurrent("c-user")
		x.forgetReads()
		x.store.CredStore = failingRead{CredStore: x.store.CredStore, fail: map[string]error{"a-user": errors.New("keychain locked")}}
		cfg, err := config.Load(config.FilePath(x.dir))
		require.NoError(t, err)
		require.NoError(t, x.store.DeleteToken(config.ServerRef{Name: "c-user", URL: cfg.Servers["c-user"].URL}))
		_, err = x.call("", "user-alice", "", nil)
		require.ErrorContains(t, err, "keychain locked", "the entry that failed might be the answer, so no later entry answers")
		assert.Equal(t, []string{"c-user", "a-user"}, x.store.reads(), "the entries after the failed one are not read")
	})
}

// TestUserIDFallsBackOnlyToItsOwnPluginLoginEntries asserts the plugin
// login entry a user entry with no login falls back to is, for a call with
// --user-id, one that records that user: one answers, none sends the call
// to a sign-in, and several refuse it, naming the login first.
func TestUserIDFallsBackOnlyToItsOwnPluginLoginEntries(t *testing.T) {
	t.Run("one of the user's", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		x.entry("prod", "", "", false)
		x.entry("exec-login-bob-entry", "bob", "user-bob", true)
		x.entry(x.pluginEntryOf("user-alice"), "alice", "user-alice", true)
		x.forgetReads()
		got, err := x.call("prod-c-test", "user-alice", "", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-alice", got)
		assert.NotContains(t, x.store.reads(), "exec-login-bob-entry", "another user's plugin login entry is not read")
	})
	t.Run("none of the user's", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		x.entry("prod", "", "", false)
		x.entry("exec-login-bob-entry", "bob", "user-bob", true)
		x.forgetReads()
		got, err := x.call("prod-c-test", "user-alice", "alice", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-alice", got, "the call signs in")
		assert.NotContains(t, x.store.reads(), "exec-login-bob-entry")
		_, own := x.stored(x.pluginEntryOf("user-alice"))
		assert.Equal(t, "user-alice", own.TokenUserID, "and makes an entry of its own")
	})
	t.Run("several of the user's", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		x.entry("prod", "", "", false)
		x.entry("exec-login-one", "alice", "user-alice", true)
		x.entry("exec-login-two", "alice", "user-alice", true)
		x.entry("exec-login-bob-entry", "bob", "user-bob", true)
		x.forgetReads()
		_, err := x.call("prod-c-test", "user-alice", "alice", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
		assert.Equal(t, "more than one login entry the exec plugin created answers for "+x.srv.URL+" ('exec-login-one', 'exec-login-two'); "+
			"pipe an API token of 'user-alice' to rancher auth login 'prod' --token-stdin to log in to 'prod'; "+
			"or keep one: for each other entry run rancher auth logout <entry>, then rancher config delete-server <entry>", msg)
		assert.Equal(t, "rancher auth login 'prod' --token-stdin", clierr.ExpandRemedies(ce.Hint, ""))
		require.Len(t, ce.Candidates, 2)
		assert.Empty(t, x.store.reads()[1:], "nothing is read after the entry the --name identifies")
	})
}

// TestSeveralPluginLoginEntriesRefuseACallWithoutUserID asserts a call
// that names no user and reaches the URL's plugin login entries is
// refused as soon as no user entry answers and there is more than one,
// before any is read and whichever hold a login; a user entry that
// answers is used and the plugin login entries are left alone.
func TestSeveralPluginLoginEntriesRefuseACallWithoutUserID(t *testing.T) {
	two := func(t *testing.T, reuse bool) *ownerFixture {
		t.Helper()
		x := newOwnerFixture(t, reuse, testrancher.Options{})
		x.entry("exec-login-one", "alice", "user-alice", true)
		x.entry("exec-login-two", "", "user-bob", true)
		x.forgetReads()
		return x
	}
	t.Run("before any is read", func(t *testing.T) {
		x := two(t, true)
		_, err := x.call("kube-c-test", "", "", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
		assert.Equal(t, "more than one login entry the exec plugin created answers for "+x.srv.URL+" ('exec-login-one', 'exec-login-two'), and this exec entry names no Rancher user; "+
			"pipe an API token to rancher auth login <name> --url '"+x.serverID()+"' --token-stdin to log in an entry for '"+x.serverID()+"', with a name of your choice for <name>; "+
			"or keep one: for each other entry run rancher auth logout <entry>, then rancher config delete-server <entry>", msg)
		assert.Empty(t, ce.Hint, "a login with a name to choose is no hint")
		assert.Empty(t, x.store.reads(), "no plugin login entry is read")
		assert.Empty(t, x.seen())
	})
	t.Run("with token reuse off they are not reached", func(t *testing.T) {
		x := two(t, false)
		_, err := x.call("kube-c-test", "", "", nil)
		ce, _ := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code, "a sign-in is asked for, not a choice among plugin login entries")
	})
	t.Run("a user entry that answers", func(t *testing.T) {
		x := two(t, true)
		x.entry("prod", "carol", "user-carol", false)
		x.forgetReads()
		got, err := x.call("kube-c-test", "", "", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-carol", got)
		assert.Equal(t, []string{"prod"}, x.store.reads(), "the plugin login entries are not read")
	})
	t.Run("a user entry with no login, the --name identifying it", func(t *testing.T) {
		x := two(t, true)
		x.entry("prod", "", "", false)
		x.forgetReads()
		_, err := x.call("prod-c-test", "", "", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
		assert.Contains(t, msg, ", and this exec entry names no Rancher user; pipe an API token to rancher auth login 'prod' --token-stdin to log in to 'prod'; or keep one: ")
		assert.Equal(t, "rancher auth login 'prod' --token-stdin", clierr.ExpandRemedies(ce.Hint, ""))
	})
	t.Run("two entries of one user", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		x.entry("exec-login-one", "alice", "user-alice", true)
		x.entry("exec-login-two", "alice", "user-alice", true)
		_, err := x.call("kube-c-test", "", "", nil)
		ce, _ := refusal(t, err)
		assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code, "the entries are counted, not their owners")
	})
	t.Run("the legacy rancher token", func(t *testing.T) {
		x := two(t, true)
		cmd := NewLegacyTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
		_, err := cmd.Execute(t.Context(), tin{s: map[string]string{"server": x.srv.URL, "user": "alice", "cluster": "c-test", "auth-provider": "localProvider"}}, ui.Discard())
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
		assert.Empty(t, x.store.reads(), "no plugin login entry is read")
	})
}

// TestUserIDWithoutAClusterAsksForATokenForRancherItself asserts --user-id
// without --cluster creates a token with no cluster, from a user entry,
// from the plugin-created login and from an interactive sign-in, whose
// token is the create the sign-in makes with the login token as bearer,
// checks its owner as every other call's, and caches it by user.
func TestUserIDWithoutAClusterAsksForATokenForRancherItself(t *testing.T) {
	scoped := func(x *ownerFixture) int {
		n := 0
		for _, c := range x.f.CreateRequests() {
			if c.ClusterName != "" {
				n++
			}
		}
		return n
	}
	t.Run("from a user entry", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		x.entry("prod", "alice", "user-alice", false)
		got, err := x.callIn("", "prod", "user-alice", "", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-alice", got)
		assert.Zero(t, scoped(x), "no create names a cluster")
		assert.True(t, x.cachedIn("", "prod", "user-alice"))
		assert.False(t, x.cachedIn("c-test", "prod", "user-alice"), "the slot is the unscoped one")
	})
	t.Run("from a user entry, no --name", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		x.entry("prod", "alice", "user-alice", false)
		got, err := x.callIn("", "", "user-alice", "", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-alice", got)
		assert.True(t, x.cachedIn("", "", "user-alice"))
	})
	t.Run("the plugin-created login", func(t *testing.T) {
		x := newOwnerFixture(t, true, testrancher.Options{})
		got, err := x.callIn("", "", "user-carol", "carol", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-carol", got)
		login, entry := x.stored(x.pluginEntryOf("user-carol"))
		assert.NotEmpty(t, login, "the lane runs without a cluster")
		assert.Equal(t, "user-carol", entry.TokenUserID)
		assert.Zero(t, scoped(x), "neither the cluster token nor the sign-in names a cluster")
		assert.True(t, x.cachedIn("", "", "user-carol"))
	})
	t.Run("an interactive sign-in", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		got, err := x.callIn("", "", "user-carol", "carol", nil)
		require.NoError(t, err)
		assert.Equal(t, "user-carol", got)
		assert.Empty(t, x.ownerReviews(), "the token's create names the owner, so no owner request is made")
		assert.Zero(t, scoped(x), "the token the sign-in creates names no cluster")
		assert.True(t, x.cachedIn("", "", "user-carol"))
	})
	t.Run("an interactive sign-in as another user", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		_, err := x.callIn("", "", "user-alice", "carol", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.True(t, strings.HasPrefix(msg, "the sign-in was made as Rancher user 'user-carol', and this exec entry names user 'user-alice' with --user-id; "), msg)
		assert.Empty(t, x.ownerReviews(), "the token's create names the owner")
		assert.GreaterOrEqual(t, x.revoked(), 1, "the token is revoked")
		assert.False(t, x.cachedIn("", "", "user-alice"))
	})
	t.Run("another user's login in the entry", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		x.entry("prod", "bob", "user-bob", false)
		_, err := x.callIn("", "prod", "user-alice", "", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Equal(t, "server entry 'prod' holds the login of Rancher user 'user-bob', and this exec entry names user 'user-alice' with --user-id; "+
			"pipe an API token of 'user-alice' to rancher auth login 'prod' --token-stdin", msg, "no kubeconfig is regenerated without a cluster")
	})
	t.Run("a created token of another user", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		x.entry("prod", "bob", "", false)
		_, err := x.callIn("", "prod", "user-alice", "", nil)
		_, msg := refusal(t, err)
		assert.True(t, strings.HasPrefix(msg, "the stored login of server entry 'prod' belongs to Rancher user 'user-bob', not to 'user-alice'"), msg)
		assert.False(t, x.cachedIn("", "prod", "user-alice"))
	})
	t.Run("without a terminal, nothing to sign in with", func(t *testing.T) {
		x := newOwnerFixture(t, false, testrancher.Options{})
		_, err := x.callIn("", "", "user-carol", "", nil)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Equal(t, "interactive login required for auth config local; this call has no terminal to answer it; to log in without one, pipe an API token of Rancher user 'user-carol' to "+
			"rancher auth login <name> --url '"+x.serverID()+"' --token-stdin, with a name of your choice for <name>; "+
			"or run kubectl once in a terminal and sign in as 'user-carol', which lasts until the cached token expires", msg)
	})
}

// TestUserIDWithoutANameIsCachedByTheUser asserts a call with --user-id
// and no --name is served from the cache - the flag fast path, with no
// "caching disabled" notice - under a key that carries the user, so two
// users never share a record; a call without either is not cached.
func TestUserIDWithoutANameIsCachedByTheUser(t *testing.T) {
	x := newOwnerFixture(t, false, testrancher.Options{})
	x.entry("alice-entry", "alice", "user-alice", false)
	x.entry("bob-entry", "bob", "user-bob", false)

	var notices bytes.Buffer
	first, err := x.call("", "user-alice", "", &notices)
	require.NoError(t, err)
	assert.Equal(t, "user-alice", first)
	assert.NotContains(t, notices.String(), "caching disabled", "a call with --user-id is cached without a --name")
	require.True(t, x.cached("", "user-alice"))
	creates := len(x.f.CreateRequests())

	notices.Reset()
	second, err := x.call("", "user-alice", "", &notices)
	require.NoError(t, err)
	assert.Equal(t, "user-alice", second)
	assert.Len(t, x.f.CreateRequests(), creates, "the second call is served from the cache")

	bob, err := x.call("", "user-bob", "", nil)
	require.NoError(t, err)
	assert.Equal(t, "user-bob", bob, "another user never shares the record")
	assert.True(t, x.cached("", "user-bob"))
	assert.True(t, x.cached("", "user-alice"))

	notices.Reset()
	_, err = x.call("", "", "", &notices)
	require.Error(t, err, "a call that names no user and sees two users' logins is refused, not served a record")
	assert.Contains(t, notices.String(), "caching disabled: no --name; each call re-authenticates", "a call with neither is not cached")
}
