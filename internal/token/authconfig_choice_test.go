package token

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
)

// TestUserIDWithAnAuthConfigServesATokenMadeThroughIt asserts a call with
// --user-id and an auth config is served a token the login of an entry that
// records that auth config made, caches it in a slot of its own - the key
// carries the auth config, so neither the slot of the call without it nor
// that of another auth config holds it - and answers the next call from the
// cache with no request.
func TestUserIDWithAnAuthConfigServesATokenMadeThroughIt(t *testing.T) {
	x := newAuthConfigFixture(t, false, testrancher.Options{})
	x.entryThrough("prod", "corp-ldap", "alice", "user-alice", "corp-ldap", false)
	call := authCall{cluster: "c-test", userID: "user-alice", authConfig: "corp-ldap"}
	tok, owner, err := x.callWith(call)
	require.NoError(t, err)
	assert.Equal(t, "user-alice", owner)
	assert.True(t, x.cachedWith("c-test", "", "user-alice", "corp-ldap"))
	assert.False(t, x.cachedWith("c-test", "", "user-alice", ""), "the slot of a call without the auth config holds nothing")
	assert.False(t, x.cachedWith("c-test", "", "user-alice", "openldap"), "nor the slot of another auth config")
	x.forgetTraffic()
	again, _, err := x.callWith(call)
	require.NoError(t, err)
	assert.Equal(t, tok, again)
	assert.Empty(t, x.seen(), "a hit makes no request")
	assert.Empty(t, x.store.reads(), "and reads no login")
}

// TestAnEntryThatRecordedNoAuthConfigRecordsItFromTheFirstToken asserts a
// stored login that recorded no auth config - a pasted token, or a login made
// before it was recorded - is a candidate of a call with an auth config, and
// when the token created from it was made through that auth config the entry
// records it, so the record the call writes lists it as it now stands and the
// next call is served from it.
func TestAnEntryThatRecordedNoAuthConfigRecordsItFromTheFirstToken(t *testing.T) {
	for _, plugin := range []bool{false, true} {
		name := "prod"
		if plugin {
			name = "exec-login-r.ex"
		}
		t.Run(name, func(t *testing.T) {
			x := newAuthConfigFixture(t, plugin, testrancher.Options{})
			x.entryThrough(name, "corp-ldap", "alice", "user-alice", "", plugin)
			call := authCall{cluster: "c-test", userID: "user-alice", authConfig: "corp-ldap"}
			tok, _, err := x.callWith(call)
			require.NoError(t, err)
			assert.Equal(t, "corp-ldap", x.entryNamed(name).TokenAuthConfig)
			assert.Equal(t, "user-alice", x.entryNamed(name).TokenUserID, "nothing else changed")
			x.forgetTraffic()
			again, _, err := x.callWith(call)
			require.NoError(t, err)
			assert.Equal(t, tok, again)
			assert.Empty(t, x.seen(), "the record written after the write lists the auth config just recorded, so it serves the next call")
		})
	}
}

// TestATokenMadeThroughAnotherAuthConfigIsRefusedAndRecorded asserts the
// check of a created token against the call's auth config: a token made
// through another is revoked with the stored login as bearer, the call fails
// and names the auth config, and the entry records the other auth config. A
// later call that names no entry then goes on to the next candidate, and one
// whose --name identifies the entry is refused before any request.
func TestATokenMadeThroughAnotherAuthConfigIsRefusedAndRecorded(t *testing.T) {
	x := newAuthConfigFixture(t, false, testrancher.Options{})
	x.entryThrough("prod", "openldap", "alice", "user-alice", "", false)
	x.entryThrough("staging", "corp-ldap", "alice", "user-alice", "", false)
	call := authCall{cluster: "c-test", name: "prod-c-test", userID: "user-alice", authConfig: "corp-ldap"}
	_, _, err := x.callWith(call)
	ce, msg := refusal(t, err)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	assert.Equal(t, "the token created from the stored login of server entry 'prod' was made through auth config 'openldap', not 'corp-ldap', which this exec entry names with --auth-provider; "+
		"the token was revoked; sign in through auth config 'corp-ldap' with rancher auth login 'prod' --auth-provider 'corp-ldap', "+
		"or pipe an API token of 'user-alice' made through auth config 'corp-ldap' to rancher auth login 'prod' --token-stdin", msg)
	assert.Equal(t, clierr.Rancher+" auth login 'prod' --auth-provider 'corp-ldap'", ce.Hint)
	assert.GreaterOrEqual(t, x.revoked(), 1, "the token is revoked")
	assert.False(t, x.cachedWith("c-test", "prod-c-test", "user-alice", "corp-ldap"))
	assert.Equal(t, "openldap", x.entryNamed("prod").TokenAuthConfig, "the entry records the auth config its token was made through")

	// A call that names no entry goes on to the next candidate.
	x.forgetTraffic()
	_, owner, err := x.callWith(authCall{cluster: "c-test", userID: "user-alice", authConfig: "corp-ldap"})
	require.NoError(t, err)
	assert.Equal(t, "user-alice", owner)
	assert.Equal(t, []string{"staging"}, x.store.reads(), "prod is no candidate now: it is not read")

	// One that identifies the entry is refused before any request.
	x.forgetTraffic()
	_, _, err = x.callWith(call)
	ce, msg = refusal(t, err)
	assert.Equal(t, clierr.CodeAuth, ce.Code)
	assert.True(t, strings.HasPrefix(msg, "server entry 'prod' holds a login made through auth config 'openldap', and this exec entry names --auth-provider 'corp-ldap'; "), msg)
	assert.Empty(t, x.seen(), "no request is made")
}

// TestAnEntryThatRecordedNoAuthConfigIsTriedAfterTheOnesThatRecordedIt
// asserts the choice by user with an auth config reads the entries that
// recorded it, then the ones that recorded none, in the fixed order within
// each, and never reads one that recorded another.
func TestAnEntryThatRecordedNoAuthConfigIsTriedAfterTheOnesThatRecordedIt(t *testing.T) {
	x := newAuthConfigFixture(t, false, testrancher.Options{})
	x.entryThrough("a-other", "openldap", "alice", "user-alice", "openldap", false)
	x.entryThrough("b-none", "corp-ldap", "alice", "user-alice", "", false)
	x.entryThrough("c-match", "corp-ldap", "alice", "user-alice", "corp-ldap", false)
	call := func(cluster string) {
		t.Helper()
		_, owner, err := x.callWith(authCall{cluster: cluster, userID: "user-alice", authConfig: "corp-ldap"})
		require.NoError(t, err)
		assert.Equal(t, "user-alice", owner)
	}
	call("c-test")
	assert.Equal(t, []string{"c-match"}, x.store.reads(), "the entry that recorded the auth config answers first")

	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(t, err)
	require.NoError(t, x.store.DeleteToken(config.ServerRef{Name: "c-match", URL: cfg.Servers["c-match"].URL}))
	x.forgetTraffic()
	call("local")
	assert.Equal(t, []string{"c-match", "b-none"}, x.store.reads(), "then the one that recorded none; the one of another auth config is never read")
}

// TestAnEntryWithNoOwnerIsNeverACandidateOfACallWithAnAuthConfig asserts an
// entry that records no owner is not read by a call with an auth config,
// whatever it records.
func TestAnEntryWithNoOwnerIsNeverACandidateOfACallWithAnAuthConfig(t *testing.T) {
	x := newAuthConfigFixture(t, false, testrancher.Options{})
	x.entryThrough("anon", "corp-ldap", "alice", "", "corp-ldap", false)
	_, _, err := x.callWith(authCall{cluster: "c-test", userID: "user-alice", authConfig: "corp-ldap"})
	require.Error(t, err)
	assert.Empty(t, x.store.reads(), "the entry is not read")
}

// TestTheFallbackCountsTheEntriesOfTheAnsweringGroup asserts the plugin login
// entries an identified user entry with no login falls back to are counted
// within the group that answers: the ones that recorded the call's auth
// config when there are any, else the ones that recorded none, and never
// one that recorded another; several in that group refuse the call.
func TestTheFallbackCountsTheEntriesOfTheAnsweringGroup(t *testing.T) {
	setup := func(t *testing.T) *ownerFixture {
		t.Helper()
		x := newAuthConfigFixture(t, true, testrancher.Options{})
		x.entryThrough("prod", "", "", "user-alice", "", false)
		x.entryThrough("exec-login-r.ex-1", "corp-ldap", "alice", "user-alice", "corp-ldap", true)
		x.entryThrough("exec-login-r.ex-2", "corp-ldap", "alice", "user-alice", "", true)
		x.entryThrough("exec-login-r.ex-3", "openldap", "alice", "user-alice", "openldap", true)
		return x
	}
	call := authCall{cluster: "c-test", name: "prod-c-test", userID: "user-alice", authConfig: "corp-ldap"}
	t.Run("one entry that recorded the auth config answers beside unrecorded ones and another's", func(t *testing.T) {
		x := setup(t)
		_, owner, err := x.callWith(call)
		require.NoError(t, err)
		assert.Equal(t, "user-alice", owner)
		assert.Equal(t, []string{"prod", "exec-login-r.ex-1"}, x.store.reads())
	})
	t.Run("two that recorded it refuse the call", func(t *testing.T) {
		x := setup(t)
		x.entryThrough("exec-login-r.ex-4", "corp-ldap", "alice", "user-alice", "corp-ldap", true)
		_, _, err := x.callWith(call)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
		assert.Contains(t, msg, "more than one login entry the exec plugin created answers for "+x.serverID()+" ('exec-login-r.ex-1', 'exec-login-r.ex-4')", "the entries of the group, and no other")
		assert.Contains(t, msg, "pipe an API token of 'user-alice' made through auth config 'corp-ldap' to rancher auth login 'prod' --token-stdin to log in to 'prod'")
		assert.Empty(t, x.seen(), "no request is made")
	})
	t.Run("with none that recorded it, one unrecorded entry answers and records the auth config", func(t *testing.T) {
		x := setup(t)
		x.entryThrough("exec-login-r.ex-1", "corp-ldap", "alice", "user-alice", "openldap", true)
		_, owner, err := x.callWith(call)
		require.NoError(t, err)
		assert.Equal(t, "user-alice", owner)
		assert.Equal(t, "corp-ldap", x.entryNamed("exec-login-r.ex-2").TokenAuthConfig)
	})
	t.Run("with none that recorded it, the unrecorded ones are counted together", func(t *testing.T) {
		x := setup(t)
		x.entryThrough("exec-login-r.ex-1", "corp-ldap", "alice", "user-alice", "openldap", true)
		x.entryThrough("exec-login-r.ex-5", "corp-ldap", "alice", "user-alice", "", true)
		_, _, err := x.callWith(call)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAmbiguousSelection, ce.Code)
		assert.Contains(t, msg, "('exec-login-r.ex-2', 'exec-login-r.ex-5')")
	})
	t.Run("an entry of another auth config that holds no login is not counted either", func(t *testing.T) {
		x := setup(t)
		x.entryThrough("exec-login-r.ex-6", "", "", "user-alice", "openldap", true)
		_, _, err := x.callWith(call)
		require.NoError(t, err)
	})
}

// TestAnIdentifiedEntryThatHoldsNoLoginIsNotJudgedByItsAuthConfig asserts an
// identified user entry with no login falls back whatever auth config it
// recorded: its recorded value plays no part where it holds nothing.
func TestAnIdentifiedEntryThatHoldsNoLoginIsNotJudgedByItsAuthConfig(t *testing.T) {
	x := newAuthConfigFixture(t, true, testrancher.Options{})
	x.entryThrough("prod", "", "", "user-alice", "openldap", false)
	x.entryThrough("exec-login-r.ex", "corp-ldap", "alice", "user-alice", "corp-ldap", true)
	_, owner, err := x.callWith(authCall{cluster: "c-test", name: "prod-c-test", userID: "user-alice", authConfig: "corp-ldap"})
	require.NoError(t, err)
	assert.Equal(t, "user-alice", owner)

	// With no plugin login entry to fall back to, the call goes on to the
	// plugin-created login, which the entry's recorded auth config does not
	// refuse either.
	y := newAuthConfigFixture(t, true, testrancher.Options{})
	y.entryThrough("prod", "", "", "user-alice", "openldap", false)
	_, owner, err = y.callWith(authCall{cluster: "c-test", name: "prod-c-test", userID: "user-alice", authConfig: "corp-ldap", signIn: signInLines("corp-ldap", "alice")})
	require.NoError(t, err)
	assert.Equal(t, "user-alice", owner)
	assert.Equal(t, "corp-ldap", y.entryNamed(config.ExecLoginEntryNameFor(y.serverID(), "user-alice", "corp-ldap")).TokenAuthConfig)
}

// TestAnIdentifiedEntryOfAnotherAuthConfigRefusesBeforeAnyRequest asserts
// the entry the --name identifies refuses a call whose auth config it
// recorded another of, when it holds a login, before any request; and a
// plugin login entry that holds none and recorded another is refused the
// same way, since it has no fallback. The owner is checked first, and an
// entry that recorded none, or the call's, goes on to the create.
func TestAnIdentifiedEntryOfAnotherAuthConfigRefusesBeforeAnyRequest(t *testing.T) {
	call := authCall{cluster: "c-test", userID: "user-alice", authConfig: "corp-ldap"}
	t.Run("a user entry that holds a login", func(t *testing.T) {
		x := newAuthConfigFixture(t, false, testrancher.Options{})
		x.entryThrough("prod", "openldap", "alice", "user-alice", "openldap", false)
		c := call
		c.name = "prod-c-test"
		_, _, err := x.callWith(c)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.Equal(t, "server entry 'prod' holds a login made through auth config 'openldap', and this exec entry names --auth-provider 'corp-ldap'; "+
			"sign in through auth config 'corp-ldap' with rancher auth login 'prod' --auth-provider 'corp-ldap', "+
			"or pipe an API token of 'user-alice' made through auth config 'corp-ldap' to rancher auth login 'prod' --token-stdin", msg)
		assert.Equal(t, clierr.Rancher+" auth login 'prod' --auth-provider 'corp-ldap'", ce.Hint)
		assert.Empty(t, x.seen(), "before any request")
	})
	t.Run("a plugin login entry that holds a login", func(t *testing.T) {
		x := newAuthConfigFixture(t, true, testrancher.Options{})
		x.entryThrough("exec-login-r.ex", "openldap", "alice", "user-alice", "openldap", true)
		c := call
		c.name = "exec-login-r.ex"
		_, _, err := x.callWith(c)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.True(t, strings.HasPrefix(msg, "server entry 'exec-login-r.ex', the exec plugin's own login entry, holds a login made through auth config 'openldap', "+
			"and this exec entry names --auth-provider 'corp-ldap'; remove 'exec-login-r.ex' with rancher auth logout 'exec-login-r.ex', then rancher config delete-server 'exec-login-r.ex'; "+
			"the next call signs in as 'user-alice' through auth config 'corp-ldap' in a terminal, or uses a login made through auth config 'corp-ldap' and piped to rancher auth login <name> --url "), msg)
		assert.Empty(t, ce.Hint)
		assert.Empty(t, x.seen())
	})
	t.Run("a plugin login entry that holds none", func(t *testing.T) {
		x := newAuthConfigFixture(t, true, testrancher.Options{})
		x.entryThrough("exec-login-r.ex", "", "", "user-alice", "openldap", true)
		c := call
		c.name = "exec-login-r.ex"
		_, _, err := x.callWith(c)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.True(t, strings.HasPrefix(msg, "server entry 'exec-login-r.ex', the exec plugin's own login entry, was last logged in through auth config 'openldap', "+
			"and this exec entry names --auth-provider 'corp-ldap'; remove 'exec-login-r.ex' with rancher auth logout 'exec-login-r.ex'"), msg)
		assert.Empty(t, x.seen())
	})
	t.Run("a plugin login entry that holds none and recorded none is written over", func(t *testing.T) {
		x := newAuthConfigFixture(t, true, testrancher.Options{})
		x.entryThrough("exec-login-r.ex", "", "", "user-alice", "", true)
		c := call
		c.name = "exec-login-r.ex"
		c.signIn = signInLines("corp-ldap", "alice")
		_, owner, err := x.callWith(c)
		require.NoError(t, err)
		assert.Equal(t, "user-alice", owner)
		assert.Equal(t, "corp-ldap", x.entryNamed("exec-login-r.ex").TokenAuthConfig)
	})
	t.Run("the owner is checked before the auth config", func(t *testing.T) {
		x := newAuthConfigFixture(t, false, testrancher.Options{})
		x.entryThrough("prod", "openldap", "bob", "user-bob", "openldap", false)
		c := call
		c.name = "prod-c-test"
		_, _, err := x.callWith(c)
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.True(t, strings.HasPrefix(msg, "server entry 'prod' holds the login of Rancher user 'user-bob', and this exec entry names user 'user-alice' with --user-id; "), msg)
	})
	t.Run("an entry that recorded the call's auth config or none goes on to the create", func(t *testing.T) {
		for _, recorded := range []string{"corp-ldap", ""} {
			x := newAuthConfigFixture(t, false, testrancher.Options{})
			x.entryThrough("prod", "corp-ldap", "alice", "user-alice", recorded, false)
			c := call
			c.name = "prod-c-test"
			_, owner, err := x.callWith(c)
			require.NoError(t, err, recorded)
			assert.Equal(t, "user-alice", owner)
		}
	})
}

// TestNoAuthConfigIsRecordedAfterAnOwnerRefusalOrOnAnEntryWithNoOwner
// asserts the write of a recorded auth config waits for the owner check and
// never goes on an entry that records no owner: a login recorded for the
// call's user that holds another user's credential is refused as such and
// records nothing; an entry the --name identifies that records no owner
// serves the token and still records none.
func TestNoAuthConfigIsRecordedAfterAnOwnerRefusalOrOnAnEntryWithNoOwner(t *testing.T) {
	t.Run("after the owner refusal", func(t *testing.T) {
		x := newAuthConfigFixture(t, false, testrancher.Options{})
		x.entryThrough("prod", "corp-ldap", "bob", "user-alice", "", false)
		_, _, err := x.callWith(authCall{cluster: "c-test", name: "prod-c-test", userID: "user-alice", authConfig: "corp-ldap"})
		ce, msg := refusal(t, err)
		assert.Equal(t, clierr.CodeAuth, ce.Code)
		assert.True(t, strings.HasPrefix(msg, "the stored login of server entry 'prod' belongs to Rancher user 'user-bob', not to 'user-alice'"), msg)
		assert.Empty(t, x.entryNamed("prod").TokenAuthConfig, "nothing is recorded")
	})
	t.Run("on an entry with no owner", func(t *testing.T) {
		x := newAuthConfigFixture(t, false, testrancher.Options{})
		x.entryThrough("prod", "corp-ldap", "alice", "", "", false)
		_, owner, err := x.callWith(authCall{cluster: "c-test", name: "prod-c-test", userID: "user-alice", authConfig: "corp-ldap"})
		require.NoError(t, err)
		assert.Equal(t, "user-alice", owner)
		assert.Empty(t, x.entryNamed("prod").TokenAuthConfig, "an auth config never goes on an entry with no owner")
	})
}

// TestARecordedAuthConfigWriteThatCannotRunLeavesTheCallAlone asserts a call
// whose write of the recorded auth config is skipped is served the token as
// ever and is not slowed: the entry's cache binding id moved after the call
// loaded, its cache writes were paused when it loaded, the config lock is
// held by another process, or the environment turns locking off. The entry
// records nothing, and a later call tries again.
func TestARecordedAuthConfigWriteThatCannotRunLeavesTheCallAlone(t *testing.T) {
	call := authCall{cluster: "c-test", userID: "user-alice", authConfig: "corp-ldap"}
	setup := func(t *testing.T) *ownerFixture {
		t.Helper()
		x := newAuthConfigFixture(t, false, testrancher.Options{})
		x.entryThrough("prod", "corp-ldap", "alice", "user-alice", "", false)
		return x
	}
	served := func(t *testing.T, x *ownerFixture) {
		t.Helper()
		_, owner, err := x.callWith(call)
		require.NoError(t, err)
		assert.Equal(t, "user-alice", owner, "the token is served")
		assert.Empty(t, x.entryNamed("prod").TokenAuthConfig, "the write was skipped")
	}
	retried := func(t *testing.T, x *ownerFixture) {
		t.Helper()
		_, _, err := x.callWith(authCall{cluster: "local", userID: "user-alice", authConfig: "corp-ldap"})
		require.NoError(t, err)
		assert.Equal(t, "corp-ldap", x.entryNamed("prod").TokenAuthConfig, "a later miss writes it")
	}
	t.Run("the binding id moved after the load", func(t *testing.T) {
		x := setup(t)
		var once sync.Once
		x.hook = func(r *http.Request) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/apis/ext.cattle.io/v1/tokens") {
				once.Do(func() {
					cfg, err := config.Load(config.FilePath(x.dir))
					require.NoError(t, err)
					s := cfg.Servers["prod"]
					require.NoError(t, config.AdvanceCacheBinding(&s))
					cfg.Servers["prod"] = s
					require.NoError(t, cfg.Write())
				})
			}
		}
		served(t, x)
		x.hook = nil
		retried(t, x)
	})
	t.Run("the load showed the entry paused", func(t *testing.T) {
		x := setup(t)
		cfg, err := config.Load(config.FilePath(x.dir))
		require.NoError(t, err)
		s := cfg.Servers["prod"]
		s.CacheWritesPaused = config.PauseLocked
		cfg.Servers["prod"] = s
		require.NoError(t, cfg.Write())
		served(t, x)
		cfg, err = config.Load(config.FilePath(x.dir))
		require.NoError(t, err)
		s = cfg.Servers["prod"]
		s.CacheWritesPaused = ""
		cfg.Servers["prod"] = s
		require.NoError(t, cfg.Write())
		retried(t, x)
	})
	t.Run("the config lock is held", func(t *testing.T) {
		x := setup(t)
		held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(done)
			_ = config.WithLock(t.Context(), x.dir, func() error {
				close(held)
				<-release
				return nil
			})
		}()
		<-held
		returned := make(chan struct{})
		go func() {
			defer close(returned)
			served(t, x)
		}()
		select {
		case <-returned:
		case <-time.After(10 * time.Second):
			t.Error("the call waited for the config lock")
		}
		close(release)
		<-done
		<-returned
		retried(t, x)
	})
	t.Run("the environment turns locking off", func(t *testing.T) {
		x := setup(t)
		t.Setenv(config.EnvConfigNoLock, "1")
		served(t, x)
		t.Setenv(config.EnvConfigNoLock, "")
		retried(t, x)
	})
	t.Run("the write fails", func(t *testing.T) {
		x := setup(t)
		restore := config.SetPersistWriteHookForTest(func(step string) error {
			if step == "recordAuthConfig" {
				return errors.New("disk full")
			}
			return nil
		})
		notices := &bytes.Buffer{}
		c := call
		c.notices = notices
		_, owner, err := x.callWith(c)
		restore()
		require.NoError(t, err, "the call does not fail")
		assert.Equal(t, "user-alice", owner)
		assert.Empty(t, x.entryNamed("prod").TokenAuthConfig)
		assert.Empty(t, notices.String(), "and prints nothing")
		retried(t, x)
	})
}

// TestACreateResponseThatNamesNoAuthConfigIsRefused asserts a token created
// from a stored login whose response names an owner and no auth config -
// nor does a v3 create, which the plugin login entry falls back to on a
// Rancher without ext tokens - cannot be checked against the call's auth
// config: it is revoked and the call fails, and nothing is recorded.
func TestACreateResponseThatNamesNoAuthConfigIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opts   testrancher.Options
		plugin bool
	}{
		{"an ext create without a principal", testrancher.Options{OmitPrincipalOnCreate: true}, false},
		{"a v3 create", testrancher.Options{Legacy: true, V3TokenCreate: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newAuthConfigFixture(t, tc.plugin, tc.opts)
			entry := "prod"
			if tc.plugin {
				entry = "exec-login-r.ex"
			}
			x.entryThrough(entry, "corp-ldap", "alice", "user-alice", "corp-ldap", tc.plugin)
			_, _, err := x.callWith(authCall{cluster: "c-test", name: entry, userID: "user-alice", authConfig: "corp-ldap"})
			ce, msg := refusal(t, err)
			assert.Equal(t, clierr.CodeAuth, ce.Code)
			assert.Equal(t, "the token create response for server entry '"+entry+"' named no auth config; it cannot be checked against --auth-provider 'corp-ldap'; the token was revoked", msg)
			assert.GreaterOrEqual(t, x.revoked(), 1)
			assert.False(t, x.cachedWith("c-test", entry, "user-alice", "corp-ldap"))
			assert.Equal(t, "corp-ldap", x.entryNamed(entry).TokenAuthConfig, "the entry is as it was")
		})
	}
}
