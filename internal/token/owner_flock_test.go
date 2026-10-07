package token

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
)

// gateReader answers a sign-in prompt with user's credentials, but only
// once released: its first read says the prompt was reached and waits.
type gateReader struct {
	once     sync.Once
	reached  chan struct{}
	released chan struct{}
	lines    *lineReader
}

func newGateReader(user string) *gateReader {
	return &gateReader{reached: make(chan struct{}), released: make(chan struct{}), lines: newLineReader(user, "pw-"+user)}
}

func (g *gateReader) Read(p []byte) (int, error) {
	g.once.Do(func() { close(g.reached) })
	<-g.released
	return g.lines.Read(p)
}

// callAside runs the exec plugin like call, for a goroutine other than the
// test's, with a terminal reading from in, and returns the token's owner.
func (x *ownerFixture) callAside(name, userID string, in *gateReader, notices *bytes.Buffer) (string, error) {
	s := map[string]string{"server": x.srv.URL, "cluster": "c-test", "name": name, "auth-provider": "localProvider"}
	if userID != "" {
		s["user-id"] = userID
	}
	cmd := NewTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
	res, err := cmd.Execute(x.t.Context(), tin{s: s}, ui.New(notices, in, false, true))
	if err != nil {
		return "", err
	}
	owner, _ := x.f.BearerOwner(res.Data.(*execCredential).Status.Token)
	return owner, nil
}

// waitingOnTheFlock arms the flock hook and returns a channel closed the
// first time a call waits for a flock after it.
func waitingOnTheFlock(t *testing.T) <-chan struct{} {
	t.Helper()
	waiting := make(chan struct{})
	var once sync.Once
	t.Cleanup(config.SetWaitForFlockHookForTest(func() { once.Do(func() { close(waiting) }) }))
	return waiting
}

// TestUserIDPluginCreatedLoginRechecksTheOwnerUnderTheFlock asserts the
// race the check under the plugin-login flock closes. A call with
// --user-id that missed while another call's plugin-created login was
// signing in waits for that flock; when the other call has logged the
// plugin login entry in as another user meanwhile - a new entry, or a
// refresh after a rejection - the call signs in interactively instead of
// writing over it, and prints no plaintext-file disclosure for a login
// that does not happen.
func TestUserIDPluginCreatedLoginRechecksTheOwnerUnderTheFlock(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "a new entry", true: "a refresh"}[refresh], func(t *testing.T) {
			x := newOwnerFixture(t, true, testrancher.Options{})
			x.entry("prod", "", "", false)
			if refresh {
				// The plugin login entry is carol's, and the server no longer
				// accepts it: both calls get a 401 and refresh.
				x.entry(x.pluginEntry(), "carol", "user-carol", true)
				login, _ := x.stored(x.pluginEntry())
				id, _, _ := strings.Cut(strings.TrimPrefix(login, "ext/"), ":")
				out := revokeToken(t.Context(), x.srv.Client(), x.srv.URL, TokenRef{Kind: TokenKindExt, Name: id}, login)
				require.Equal(t, ResultRevoked, out.Result, "the server no longer accepts the login")
			}

			other := newGateReader("bob")
			var otherNotices bytes.Buffer
			otherDone := make(chan error, 1)
			go func() {
				_, err := x.callAside("prod-c-test", "", other, &otherNotices)
				otherDone <- err
			}()
			// A call that ends before its prompt fails the test at once
			// instead of leaving it waiting for a prompt that never comes.
			select {
			case <-other.reached: // the other call holds the flock, at its sign-in prompt
			case err := <-otherDone:
				require.FailNow(t, "the other call ended before its sign-in prompt", "%v", err)
			}

			waiting := waitingOnTheFlock(t)
			mine := newGateReader("carol")
			close(mine.released)
			var notices bytes.Buffer
			type result struct {
				owner string
				err   error
			}
			done := make(chan result, 1)
			go func() {
				owner, err := x.callAside("prod-c-test", "user-carol", mine, &notices)
				done <- result{owner, err}
			}()
			select {
			case <-waiting:
			case got := <-done:
				close(other.released)
				require.FailNow(t, "the call ended without waiting for the flock", "%v", got.err)
			}
			close(other.released)
			require.NoError(t, <-otherDone)

			got := <-done
			require.NoError(t, got.err)
			assert.Equal(t, "user-carol", got.owner, "the call signs in as its own user")
			login, s := x.stored(x.pluginEntry())
			owner, _ := x.f.BearerOwner(login)
			assert.Equal(t, "user-bob", owner, "the other call's login is not written over")
			assert.Equal(t, "user-bob", s.TokenUserID)
			assert.NotContains(t, notices.String(), "plaintext file", "no disclosure for a login that does not happen")
			assert.Contains(t, otherNotices.String(), "plaintext file", "the other call's login disclosed it")
		})
	}
}

// TestUserIDSkipTakesNoPluginLoginFlock asserts a call that skips another
// user's plugin login entry never enters the plugin lane: with the
// plugin-login flock held by another call at a sign-in prompt, it reaches
// its own sign-in at once.
func TestUserIDSkipTakesNoPluginLoginFlock(t *testing.T) {
	for _, name := range []string{"prod-c-test", "kube-c-test"} {
		t.Run(name, func(t *testing.T) {
			x := newOwnerFixture(t, true, testrancher.Options{})
			if name == "prod-c-test" {
				x.entry("prod", "", "", false)
			}
			x.entry(x.pluginEntry(), "bob", "user-bob", true)

			held := make(chan struct{})
			release := make(chan struct{})
			lockDone := make(chan error, 1)
			go func() {
				lockDone <- config.WithPluginLoginLock(t.Context(), x.dir, x.serverID(), func() error {
					close(held)
					<-release
					return nil
				})
			}()
			<-held
			t.Cleanup(func() {
				close(release)
				require.NoError(t, <-lockDone)
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			s := map[string]string{"server": x.srv.URL, "cluster": "c-test", "name": name, "auth-provider": "localProvider", "user-id": "user-carol"}
			cmd := NewTokenCommand(x.dir, x.store, anchorFor(x.dir)).(runtime.ReadCommand)
			res, err := cmd.Execute(ctx, tin{s: s}, ui.New(&bytes.Buffer{}, newLineReader("carol", "pw-carol"), false, true))
			require.NoError(t, err, "the call does not wait for the flock, which it would hold for a minute")
			owner, _ := x.f.BearerOwner(res.Data.(*execCredential).Status.Token)
			assert.Equal(t, "user-carol", owner)
		})
	}
}

// failingCommit is a store whose credential commit runs before first,
// then refuses.
type failingCommit struct {
	config.CredStore
	before func()
}

func (s failingCommit) SetTokenWithOutcome(config.ServerRef, string, config.PriorCredential) (config.CommitOutcome, error) {
	s.before()
	return config.CommitOutcome{}, errors.New("the store refused the write")
}

// TestUserIDBesideAFailedLoginOfItsEntry asserts what a call with
// --user-id sees while a login of its entry as another user fails: before
// that login's credential commit and after it, the entry records the new
// owner and its cache writes are paused, so a call for the earlier owner
// is refused, and a call for the new one is refused before the commit
// (the token made from the earlier login is not the new owner's) and gets
// the new login's token after it, writing nothing either way; once the
// login has undone its writes, the record made before it is not served
// again, and the next call makes its own.
func TestUserIDBesideAFailedLoginOfItsEntry(t *testing.T) {
	m := newMadeFrom(t)
	userKey := func(dir, userID string) string {
		return config.CacheKeyFor("prod", config.CanonicalServerIdentity(m.URL), "local", userID)
	}
	servedFor := func(t *testing.T, dir, userID string) bool {
		t.Helper()
		_, store, err := config.LoadAndStore(dir)
		require.NoError(t, err)
		_, _, err = store.GetCachedCred(userKey(dir, userID), anchorFor(dir))
		return err == nil
	}
	// written reports whether a record is on disk in the slot, servable
	// or not: a read refuses every record while the entry is paused.
	written := func(t *testing.T, dir, userID string) bool {
		t.Helper()
		infos, err := config.ReadOnlyCachedCreds(dir)
		require.NoError(t, err)
		return slices.ContainsFunc(infos, func(i config.CachedCredInfo) bool { return i.Key == userKey(dir, userID) })
	}
	during := func(t *testing.T, dir string, committed bool) {
		t.Helper()
		_, err := kubectl(t, dir, m.URL, ui.Discard(), nil, "user-id", "user-alice")
		ce, ok := errors.AsType[*clierr.Error](err)
		require.True(t, ok, "the earlier owner's call is refused: %v", err)
		assert.Contains(t, ce.Message, "holds the login of Rancher user 'user-bob'")
		before := written(t, dir, "user-alice")
		tok, err := kubectl(t, dir, m.URL, ui.Discard(), nil, "user-id", "user-bob")
		if committed {
			require.NoError(t, err)
			assert.Equal(t, testrancher.MadeFromToken("bob"), tok, "the new owner's call gets a token of its login")
		} else {
			require.ErrorContains(t, err, "belongs to Rancher user 'user-alice'", "the earlier login's token is refused for the new owner")
		}
		assert.False(t, written(t, dir, "user-bob"), "nothing is written while the entry is paused")
		assert.Equal(t, before, written(t, dir, "user-alice"))
		assert.False(t, servedFor(t, dir, "user-alice"), "and the earlier owner's record is not served")
	}
	setup := func(t *testing.T) string {
		t.Helper()
		dir := changeDir(t, m.URL, "file", "")
		_, err := loginAs(t, dir, m.URL, "alice", func(in *config.PersistLoginInput) { in.TokenUserID = "user-alice" })
		require.NoError(t, err)
		tok, err := kubectl(t, dir, m.URL, ui.Discard(), nil, "user-id", "user-alice")
		require.NoError(t, err)
		require.Equal(t, testrancher.MadeFromToken("alice"), tok)
		require.True(t, servedFor(t, dir, "user-alice"))
		return dir
	}
	after := func(t *testing.T, dir string) {
		t.Helper()
		assert.False(t, servedFor(t, dir, "user-alice"), "the record made before the failed login is not served")
		tok, err := kubectl(t, dir, m.URL, ui.Discard(), nil, "user-id", "user-alice")
		require.NoError(t, err)
		assert.Equal(t, testrancher.MadeFromToken("alice"), tok)
		assert.True(t, servedFor(t, dir, "user-alice"), "the next call caches its own")
	}

	t.Run("before its commit", func(t *testing.T) {
		dir := setup(t)
		_, err := loginAs(t, dir, m.URL, "bob", func(in *config.PersistLoginInput) {
			in.TokenUserID = "user-bob"
			in.OpenStore = func(string, string) (config.CredStore, string, error) {
				return failingCommit{CredStore: config.NewFileStore(dir), before: func() { during(t, dir, false) }}, "", nil
			}
		})
		require.Error(t, err)
		after(t, dir)
	})
	t.Run("after its commit", func(t *testing.T) {
		dir := setup(t)
		t.Cleanup(config.SetPersistWriteHookForTest(func(step string) error {
			if step == "trust" {
				// The credential has committed: the entry holds bob's
				// login, records bob and is paused until the login ends.
				during(t, dir, true)
				return errors.New("config directory is full")
			}
			return nil
		}))
		_, err := loginAs(t, dir, m.URL, "bob", func(in *config.PersistLoginInput) {
			in.TokenUserID = "user-bob"
			in.PendingTrust = &config.PendingTrust{CA: "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n", Fingerprint: "f1"}
		})
		require.Error(t, err)
		config.SetPersistWriteHookForTest(nil)
		after(t, dir)
	})
}
