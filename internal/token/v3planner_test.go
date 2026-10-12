package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// v3Options is the fixture Rancher the v3 tests run on: a standard user's
// proxy delete of a v3 token answers 403, as Rancher's does.
var v3Options = testrancher.Options{V3DeleteForbiddenForNonAdmin: true}

// v3Token creates a live unscoped v3 token of user, as a legacy sign-in
// does: the login answers with the v3 body, and the token is not replaced
// by a derived one.
func (x *revFixture) v3Token(user string) LoginCredential {
	x.t.Helper()
	body, err := json.Marshal(map[string]string{"type": "localProvider", "username": user, "password": "pw", "responseType": "json"})
	require.NoError(x.t, err)
	req, err := http.NewRequestWithContext(x.t.Context(), http.MethodPost, x.url+"/v1-public/login", bytes.NewReader(body))
	require.NoError(x.t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, raw, err := httpx.Do(x.srv.Client(), req)
	require.NoError(x.t, err)
	require.Equal(x.t, http.StatusCreated, resp.StatusCode)
	var out struct {
		ID        string `json:"id"`
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
	}
	require.NoError(x.t, json.Unmarshal(raw, &out))
	return LoginCredential{Token: out.Token, ExpiresAt: out.ExpiresAt, Provenance: ProvenanceLegacySession, UserID: "user-" + user,
		Ref: &TokenRef{Kind: TokenKindV3, Name: out.ID}}
}

// v3Login gives entry a v3 login of user, as auth login stores one when
// the server serves no ext tokens.
func (x *revFixture) v3Login(name, user string) LoginCredential {
	x.t.Helper()
	cred := x.v3Token(user)
	x.entry(name, config.Server{Provenance: clierr.ProvenanceLegacySession, TokenRef: cred.Ref, TokenUserID: cred.UserID, CredentialExpiresAt: cred.ExpiresAt})
	require.NoError(x.t, x.store.SetToken(config.ServerRef{Name: name, URL: x.url}, cred.Token))
	return cred
}

// cacheOn writes a cache record of key for cluster under b naming cred's
// token; an empty cluster is the record of a legacy `rancher token` call
// with no --cluster.
func (x *revFixture) cacheOn(key string, cred LoginCredential, cluster string, b config.CacheBinding) {
	x.t.Helper()
	b = b.WithCreatedToken(cred.Ref, cred.UserID)
	require.NoError(x.t, x.store.SetCachedCred(key, cred.Token, time.Now().Add(time.Hour), key, config.CanonicalServerIdentity(x.url), cluster, b))
}

// requestsFor lists the logout requests and the DELETE requests the
// fixture Rancher saw for the token name.
func (x *revFixture) requestsFor(name string) (logouts, deletes []testrancher.DeleteRecord) {
	for _, r := range x.f.DeleteRequests() {
		switch {
		case r.Name != name:
		case r.Route == testrancher.RouteLogout:
			logouts = append(logouts, r)
		default:
			deletes = append(deletes, r)
		}
	}
	return logouts, deletes
}

// deleteTokenReq is auth delete-token's request for every cache record,
// which reads and deletes no login.
func (x *revFixture) deleteTokenReq() LocalDelete {
	req := x.req(DeleteForDeleteToken)
	req.Select = func(config.CachedRecord) bool { return true }
	req.DeleteLogin = func(context.Context, config.ServerRef) ([]string, []string, error) {
		return nil, nil, errors.New("delete-token deletes no login")
	}
	return req
}

// TestAnUnscopedV3CacheTokenIsDeletedWithItsOwnBearer asserts the four
// deleting commands delete an unscoped v3 cache token through one logout
// request with its own bearer, without a warning and without a record left.
func TestAnUnscopedV3CacheTokenIsDeletedWithItsOwnBearer(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func(x *revFixture) LocalDelete
	}{
		{"auth logout", func(x *revFixture) LocalDelete { return x.req(DeleteForLogout, "prod") }},
		{"auth logout --all", func(x *revFixture) LocalDelete { return x.req(DeleteForLogoutAll, "prod") }},
		{"config delete-server", func(x *revFixture) LocalDelete { return x.req(DeleteForDeleteServer, "prod") }},
		{"auth delete-token", func(x *revFixture) LocalDelete { return x.deleteTokenReq() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newRevFixture(t, v3Options)
			x.login("prod", "alice")
			ct := x.v3Token("alice")
			x.cacheOn("k", ct, "", x.own("prod"))

			d, rep, err := x.run(tc.req(x))
			require.NoError(t, err)
			assert.Contains(t, revokedNames(rep), ct.Ref.Name)
			assert.Empty(t, rep.NotRevoked)
			assert.Empty(t, d.Warnings())
			logouts, deletes := x.requestsFor(ct.Ref.Name)
			require.Len(t, logouts, 1)
			assert.Equal(t, ct.Token, logouts[0].Bearer)
			assert.Empty(t, deletes, "logout deleted it; no delete follows")
			assert.False(t, x.alive(ct))
			assert.Empty(t, x.load().Servers["prod"].SupersededTokens)
			assert.Empty(t, x.cacheKeys())
		})
	}
}

// TestAV3CacheTokenThatIsNotUnscopedWithItsBearerIsNotSent asserts a v3
// cache token with a cluster, one whose name no v3 object can have, one
// whose token does not match its name, and one with no entry to send
// through keep their reasons and send nothing.
func TestAV3CacheTokenThatIsNotUnscopedWithItsBearerIsNotSent(t *testing.T) {
	x := newRevFixture(t, v3Options)
	x.login("prod", "alice")
	scoped, mismatch := x.v3Token("alice"), x.v3Token("alice")
	x.cacheOn("scoped", scoped, "c-1", x.own("prod"))
	b := x.own("prod").WithCreatedToken(&TokenRef{Kind: TokenKindV3, Name: "someone-else"}, "user-alice")
	require.NoError(t, x.store.SetCachedCred("mismatch", mismatch.Token, time.Now().Add(time.Hour), "mismatch", x.url, "", b))
	badName := LoginCredential{Token: "Bad_Name:secret", Ref: &TokenRef{Kind: TokenKindV3, Name: "Bad_Name"}, UserID: "user-alice"}
	x.cacheOn("badname", badName, "", x.own("prod"))
	before := len(x.f.DeleteRequests())

	_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	n := notRevoked(t, rep, scoped.Ref.Name)
	assert.Equal(t, "v3", n.Reason, "a cluster-scoped token is refused by Rancher on every path that names no cluster")
	assert.Equal(t, "manual", n.Retry)
	assert.Equal(t, []string{v3AdminDeleteCommand(scoped.Ref.Name)}, n.Recovery)
	assert.Equal(t, "prod", n.Entry)
	assert.Equal(t, "noBearer", notRevoked(t, rep, "someone-else").Reason)
	n = notRevoked(t, rep, "Bad_Name")
	assert.Equal(t, "v3", n.Reason, "a name the v3 pattern rejects is never sent")
	assert.Equal(t, []string{v3AdminDeleteCommand("Bad_Name")}, n.Recovery)
	for _, d := range x.f.DeleteRequests()[before:] {
		assert.NotContains(t, []string{scoped.Ref.Name, mismatch.Ref.Name, "Bad_Name", "someone-else"}, d.Name)
	}
	assert.True(t, x.alive(scoped))
}

// TestAV3CacheTokenWithNoEntryIsNoEntry asserts a v3 cache token no entry
// names keeps reason noEntry and the administrator's delete.
func TestAV3CacheTokenWithNoEntryIsNoEntry(t *testing.T) {
	x := newRevFixture(t, v3Options)
	x.entry("prod", config.Server{Provenance: clierr.ProvenanceDerived, TokenUserID: "user-alice"})
	ct := x.v3Token("alice")
	x.cacheOn("k", ct, "", x.own("prod"))
	x.edit(func(cfg *config.Config) { delete(cfg.Servers, "prod") })
	before := len(x.f.DeleteRequests())

	_, rep, err := x.run(x.req(DeleteForLogoutAll))
	require.NoError(t, err)
	n := notRevoked(t, rep, ct.Ref.Name)
	assert.Equal(t, "noEntry", n.Reason)
	assert.Equal(t, "manual", n.Retry)
	assert.Equal(t, []string{v3AdminDeleteCommand(ct.Ref.Name)}, n.Recovery)
	assert.Len(t, x.f.DeleteRequests(), before)
}

// TestUpdateServerKeepsEveryV3Token asserts config update-server sends
// nothing for a v3 token of either scope or a v3 login: each keeps reason
// v3 with the administrator's delete, because the record it moves to holds
// no bearer.
func TestUpdateServerKeepsEveryV3Token(t *testing.T) {
	x := newRevFixture(t, v3Options)
	login := x.v3Login("prod", "alice")
	unscoped, scoped := x.v3Token("alice"), x.v3Token("alice")
	x.cacheOn("unscoped", unscoped, "", x.own("prod"))
	x.cacheOn("scoped", scoped, "c-1", x.own("prod"))
	before := len(x.f.DeleteRequests())

	d, err := x.localDelete(x.req(DeleteForUpdateServer, "prod"))
	require.NoError(t, err)
	d.RevokeTokens(t.Context())
	assert.Empty(t, d.Settle(t.Context()))
	rep := d.Report()
	assert.Len(t, x.f.DeleteRequests(), before, "update-server sends nothing")
	assert.Empty(t, rep.Revoked)
	for _, c := range []LoginCredential{login, unscoped, scoped} {
		n := notRevoked(t, rep, c.Ref.Name)
		assert.Equal(t, "v3", n.Reason, c.Ref.Name)
		assert.Equal(t, "manual", n.Retry, c.Ref.Name)
		assert.Equal(t, []string{v3AdminDeleteCommand(c.Ref.Name)}, n.Recovery, c.Ref.Name)
		assert.True(t, x.alive(c))
	}
}

// TestAV3LoginIsDeletedWithItselfLast asserts a v3 login with its stored
// bearer is revoked through logout after the cache tokens, and that one
// with no stored login is noBearer with the administrator's delete.
func TestAV3LoginIsDeletedWithItselfLast(t *testing.T) {
	t.Run("a stored login", func(t *testing.T) {
		x := newRevFixture(t, v3Options)
		login := x.v3Login("prod", "alice")
		ct := x.v3Token("alice")
		x.cacheOn("k", ct, "", x.own("prod"))

		d, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		assert.Equal(t, []string{ct.Ref.Name, login.Ref.Name}, revokedNames(rep))
		assert.Empty(t, rep.NotRevoked)
		assert.Empty(t, d.Warnings())
		logouts, deletes := x.requestsFor(login.Ref.Name)
		require.Len(t, logouts, 1)
		assert.Equal(t, login.Token, logouts[0].Bearer)
		assert.Empty(t, deletes)
		assert.False(t, x.alive(login))
		assert.Empty(t, x.load().Servers["prod"].SupersededTokens)
	})
	t.Run("no stored login", func(t *testing.T) {
		x := newRevFixture(t, v3Options)
		cred := x.v3Token("alice")
		x.entry("prod", config.Server{Provenance: clierr.ProvenanceLegacySession, TokenRef: cred.Ref, TokenUserID: cred.UserID})
		before := len(x.f.DeleteRequests())

		d, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		n := notRevoked(t, rep, cred.Ref.Name)
		assert.Equal(t, "noBearer", n.Reason)
		assert.Equal(t, "manual", n.Retry, "no later login deletes a v3 token")
		assert.Equal(t, []string{v3AdminDeleteCommand(cred.Ref.Name)}, n.Recovery)
		assert.Len(t, x.f.DeleteRequests(), before)
		assert.NotContains(t, d.Warnings(), "revoked by the next login")
		assert.True(t, x.alive(cred))
	})
	t.Run("a name the v3 pattern rejects", func(t *testing.T) {
		x := newRevFixture(t, v3Options)
		x.entry("prod", config.Server{Provenance: clierr.ProvenanceLegacySession, TokenRef: &TokenRef{Kind: TokenKindV3, Name: "Bad_Name"}, TokenUserID: "user-alice"})
		require.NoError(t, x.store.SetToken(config.ServerRef{Name: "prod", URL: x.url}, "Bad_Name:secret"))
		before := len(x.f.DeleteRequests())

		_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		assert.Equal(t, "v3", notRevoked(t, rep, "Bad_Name").Reason)
		assert.Len(t, x.f.DeleteRequests(), before)
	})
}

// TestAV3SupersededRecordOfTheStoredLogin asserts the record a logout
// leaves when its login delete failed: a v3 record matching the stored
// login is revoked with that login as its own bearer once a run deletes
// the login, is notDeleted with the command to run again while the login
// stays stored, and is never matched by a command that reads no login.
func TestAV3SupersededRecordOfTheStoredLogin(t *testing.T) {
	loggedOut := func(x *revFixture) LoginCredential {
		login := x.v3Login("prod", "alice")
		x.edit(func(cfg *config.Config) {
			s := cfg.Servers["prod"]
			s.TokenRef, s.LoggedOutAt = nil, "2026-10-01T00:00:00Z"
			s.SupersededTokens = []config.SupersededToken{{Ref: *login.Ref, UserID: "user-alice", SupersededAt: "2026-10-01T00:00:00Z"}}
			cfg.Servers["prod"] = s
		})
		return login
	}
	t.Run("the run deletes the login", func(t *testing.T) {
		x := newRevFixture(t, v3Options)
		login := loggedOut(x)
		d, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		assert.Equal(t, []string{login.Ref.Name}, revokedNames(rep))
		assert.Empty(t, rep.NotRevoked)
		assert.Empty(t, d.Warnings())
		logouts, deletes := x.requestsFor(login.Ref.Name)
		require.Len(t, logouts, 1)
		assert.Equal(t, login.Token, logouts[0].Bearer, "the record is sent with the stored login as its own bearer")
		assert.Empty(t, deletes)
		assert.Empty(t, x.load().Servers["prod"].SupersededTokens)
	})
	t.Run("the login stays stored", func(t *testing.T) {
		x := newRevFixture(t, v3Options)
		login := loggedOut(x)
		failing := x.req(DeleteForLogout, "prod")
		failing.DeleteLogin = func(context.Context, config.ServerRef) ([]string, []string, error) {
			return nil, nil, errors.New("the helper refused")
		}
		before := len(x.f.DeleteRequests())
		d, rep, err := x.run(failing)
		require.Error(t, err)
		n := notRevoked(t, rep, login.Ref.Name)
		assert.Equal(t, "notDeleted", n.Reason)
		assert.Equal(t, "manual", n.Retry)
		assert.Equal(t, []string{"rancher auth logout"}, n.Recovery, "the rerun deletes it, not the administrator")
		assert.Contains(t, d.Warnings()[0], "its local copy was not deleted and it stays in use; run "+clierr.Rancher+" auth logout again")
		assert.Len(t, x.f.DeleteRequests(), before)
		assert.True(t, x.alive(login))
	})
	t.Run("a command that reads no login", func(t *testing.T) {
		x := newRevFixture(t, v3Options)
		login := loggedOut(x)
		before := len(x.f.DeleteRequests())
		req := x.req(DeleteForDeleteToken, "prod")
		req.Select = func(config.CachedRecord) bool { return true }
		_, rep, err := x.run(req)
		require.NoError(t, err)
		assert.Empty(t, rep.Revoked)
		assert.Equal(t, "v3", notRevoked(t, rep, login.Ref.Name).Reason, "no login is read, so the record matches none")
		assert.Len(t, x.f.DeleteRequests(), before)
		assert.True(t, x.alive(login))
	})
	t.Run("a stored login whose name the v3 pattern rejects", func(t *testing.T) {
		x := newRevFixture(t, v3Options)
		ref := TokenRef{Kind: TokenKindV3, Name: "Bad_Name"}
		x.entry("prod", config.Server{Provenance: clierr.ProvenanceLegacySession, TokenUserID: "user-alice", LoggedOutAt: "2026-10-01T00:00:00Z",
			SupersededTokens: []config.SupersededToken{{Ref: ref, UserID: "user-alice", SupersededAt: "2026-10-01T00:00:00Z"}}})
		require.NoError(t, x.store.SetToken(config.ServerRef{Name: "prod", URL: x.url}, "Bad_Name:secret"))
		before := len(x.f.DeleteRequests())
		_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		n := notRevoked(t, rep, "Bad_Name")
		assert.Equal(t, "v3", n.Reason)
		assert.Equal(t, []string{v3AdminDeleteCommand("Bad_Name")}, n.Recovery)
		assert.Len(t, x.f.DeleteRequests(), before)
	})
	t.Run("another v3 record is never sent", func(t *testing.T) {
		x := newRevFixture(t, v3Options)
		login := loggedOut(x)
		other := x.v3Token("alice")
		x.edit(func(cfg *config.Config) {
			s := cfg.Servers["prod"]
			s.SupersededTokens = append(s.SupersededTokens, config.SupersededToken{Ref: *other.Ref, UserID: "user-alice", SupersededAt: "2026-10-01T00:00:00Z"})
			cfg.Servers["prod"] = s
		})
		_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		assert.Equal(t, []string{login.Ref.Name}, revokedNames(rep))
		n := notRevoked(t, rep, other.Ref.Name)
		assert.Equal(t, "v3", n.Reason)
		assert.Equal(t, []string{v3AdminDeleteCommand(other.Ref.Name)}, n.Recovery)
		assert.True(t, x.alive(other))
	})
}

// TestAFailedRestoreOfAV3LoginAndTheRerun asserts a v3 login whose delete
// failed with its restore: the first run reports it notDeleted, the next
// run that still cannot delete the login reports its record notDeleted
// too, and the run that deletes the login revokes it.
func TestAFailedRestoreOfAV3LoginAndTheRerun(t *testing.T) {
	x := newRevFixture(t, v3Options)
	login := x.v3Login("prod", "alice")
	refuse := func(context.Context, config.ServerRef) ([]string, []string, error) {
		return nil, nil, errors.New("the helper refused")
	}
	failing := x.req(DeleteForLogout, "prod")
	failing.DeleteLogin = refuse
	_, rep, err := x.run(failing)
	require.Error(t, err)
	assert.Equal(t, "notDeleted", notRevoked(t, rep, login.Ref.Name).Reason)
	assert.True(t, x.alive(login))

	_, rep, err = x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	assert.Equal(t, []string{login.Ref.Name}, revokedNames(rep))
	assert.False(t, x.alive(login))
}

// TestAV3TokenNotDeletedIsManualWithTheAdministratorsDelete asserts a v3
// login whose logout was refused is reported retry manual, never nextLogin,
// with the administrator's delete as its recovery and a warning that names
// that delete and no assumption about the environment, and that the next
// login does not count it.
func TestAV3TokenNotDeletedIsManualWithTheAdministratorsDelete(t *testing.T) {
	for _, tc := range []struct {
		name, mode, reason, reasonText string
	}{
		{"logout 401", "gone", "unconfirmed", "unconfirmed"},
		{"logout 500", "fail", "failed", "failed: logout 500; delete 403 Forbidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newRevFixture(t, v3Options)
			login := x.v3Login("prod", "alice")
			require.NotEmpty(t, x.load().Servers["prod"].TokenUserID, "the owner is known")
			x.f.SetDeleteMode(login.Ref.Name, tc.mode)

			d, rep, err := x.run(x.req(DeleteForLogout, "prod"))
			require.NoError(t, err)
			n := notRevoked(t, rep, login.Ref.Name)
			assert.Equal(t, tc.reason, n.Reason)
			assert.Equal(t, "manual", n.Retry)
			assert.Equal(t, []string{v3AdminDeleteCommand(login.Ref.Name)}, n.Recovery)
			assert.Equal(t, []string{"the login token " + login.Ref.String() + " was not revoked (" + tc.reasonText + "); " +
				v3AdminRemedy(*login.Ref, login.ExpiresAt)}, d.Warnings())
			assert.NotContains(t, d.Warnings()[0], clierr.RecoverySequenceAssumption)
			assert.NotContains(t, d.Warnings()[0], "revoked by the next login")
		})
	}
}

// TestAV3ItemThatEndsWithAnotherReasonGetsTheAdministratorsDelete asserts a
// v3 item reported budget or cancelled is manual with the administrator's
// delete, not the owner's login sequence.
func TestAV3ItemThatEndsWithAnotherReasonGetsTheAdministratorsDelete(t *testing.T) {
	t.Run("budget", func(t *testing.T) {
		old := revocationBudget
		revocationBudget = 300 * time.Millisecond
		t.Cleanup(func() { revocationBudget = old })
		x := newRevFixture(t, v3Options)
		login := x.v3Login("prod", "alice")
		ct := x.v3Token("alice")
		x.cacheOn("k", ct, "", x.own("prod"))
		x.f.SetDeleteMode(ct.Ref.Name, "hang")
		_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		n := notRevoked(t, rep, login.Ref.Name)
		assert.Equal(t, "budget", n.Reason)
		assert.Equal(t, "manual", n.Retry)
		assert.Equal(t, []string{v3AdminDeleteCommand(login.Ref.Name)}, n.Recovery)
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		var mu sync.Mutex
		fired := false
		opts := v3Options
		opts.RequestHook = func(r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if !fired && r.URL.Path == LogoutPath {
				fired = true
				cancel()
			}
		}
		x := newRevFixture(t, opts)
		login := x.v3Login("prod", "alice")
		ct := x.v3Token("alice")
		x.cacheOn("k", ct, "", x.own("prod"))
		d, err := x.localDelete(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		d.RevokeTokens(ctx)
		require.Empty(t, d.Settle(ctx))
		n := notRevoked(t, d.Report(), login.Ref.Name)
		assert.Equal(t, "cancelled", n.Reason)
		assert.Equal(t, "manual", n.Retry)
		assert.Equal(t, []string{v3AdminDeleteCommand(login.Ref.Name)}, n.Recovery)
	})
}

// TestAnUnconfirmedV3CacheTokenIsNotRetriedWithTheLogin asserts a v3 cache
// token whose own bearer no longer authenticates is deleted by one logout
// and one delete, both with its own bearer: the stored login of the same
// owner is never used for it, because a standard user's delete of a v3 token
// with another token's bearer always answers 403.
func TestAnUnconfirmedV3CacheTokenIsNotRetriedWithTheLogin(t *testing.T) {
	x := newRevFixture(t, v3Options)
	login := x.login("prod", "alice")
	ct := x.v3Token("alice")
	x.cacheOn("k", ct, "", x.own("prod"))
	x.f.SetDeleteMode(ct.Ref.Name, "gone")

	_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	n := notRevoked(t, rep, ct.Ref.Name)
	assert.Equal(t, "unconfirmed", n.Reason)
	logouts, deletes := x.requestsFor(ct.Ref.Name)
	require.Len(t, logouts, 1)
	require.Len(t, deletes, 1, "one delete, with its own bearer; the login's retry is skipped")
	assert.Equal(t, ct.Token, logouts[0].Bearer)
	assert.Equal(t, ct.Token, deletes[0].Bearer)
	assert.NotEqual(t, login.Token, deletes[0].Bearer)
}

// TestAV3LoginIsTheBearerOfAnExtRecordBeforeItsOwnLogout asserts an ext
// superseded record of the stored v3 login's owner is revoked with that
// login as its bearer before the login's own logout.
func TestAV3LoginIsTheBearerOfAnExtRecordBeforeItsOwnLogout(t *testing.T) {
	x := newRevFixture(t, v3Options)
	login := x.v3Login("prod", "alice")
	old := x.token("alice")
	x.edit(func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		s.SupersededTokens = []config.SupersededToken{{Ref: *old.Ref, UserID: "user-alice", SupersededAt: "2026-10-01T00:00:00Z"}}
		cfg.Servers["prod"] = s
	})
	before := len(x.f.DeleteRequests())

	_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	assert.Equal(t, []string{old.Ref.Name, login.Ref.Name}, revokedNames(rep))
	reqs := x.f.DeleteRequests()[before:]
	require.Len(t, reqs, 2)
	assert.Equal(t, old.Ref.Name, reqs[0].Name)
	assert.Equal(t, login.Token, reqs[0].Bearer, "the record is sent with the login as its bearer")
	assert.Equal(t, testrancher.RouteLogout, reqs[1].Route)
	assert.Equal(t, login.Token, reqs[1].Bearer)
}

// TestTwoLogoutsOfOneV3CacheTokenSendOneLogout asserts two concurrent
// auth logout runs on one directory send one logout for an unscoped v3
// cache token: the second run finds no cache record, and the record the
// first run left for it is either gone or sent nothing for.
func TestTwoLogoutsOfOneV3CacheTokenSendOneLogout(t *testing.T) {
	x := newRevFixture(t, v3Options)
	x.login("prod", "alice")
	ct := x.v3Token("alice")
	x.cacheOn("k", ct, "", x.own("prod"))

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() {
			d, err := x.localDelete(x.req(DeleteForLogout, "prod"))
			if d != nil {
				d.RevokeTokens(t.Context())
				d.Settle(t.Context())
			}
			errs[i] = err
		})
	}
	wg.Wait()
	assert.NoError(t, errs[0])
	assert.NoError(t, errs[1])
	logouts, _ := x.requestsFor(ct.Ref.Name)
	assert.Len(t, logouts, 1, "one logout request for the token in total")
	assert.False(t, x.alive(ct))
}

// TestCachedTokenText asserts a cache token with no cluster reads "cached
// token" in the text lines and the warnings, a v3 or an ext one alike, and
// one with a cluster reads as a cluster token.
func TestCachedTokenText(t *testing.T) {
	x := newRevFixture(t, v3Options)
	x.login("prod", "alice")
	v3Unscoped, v3Failed, v3Scoped, extUnscoped, extScoped := x.v3Token("alice"), x.v3Token("alice"), x.v3Token("alice"), x.token("alice"), x.token("alice")
	x.cacheOn("v3-unscoped", v3Unscoped, "", x.own("prod"))
	x.cacheOn("v3-failed", v3Failed, "", x.own("prod"))
	x.cacheOn("v3-scoped", v3Scoped, "c-1", x.own("prod"))
	x.cacheOn("ext-unscoped", extUnscoped, "", x.own("prod"))
	x.cacheOn("ext-scoped", extScoped, "c-1", x.own("prod"))
	x.f.SetDeleteMode(v3Failed.Ref.Name, "fail")

	d, _, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	lines := d.TextLines()
	assert.Contains(t, lines, "  revoked "+v3Unscoped.Ref.String()+" (cached token)")
	assert.Contains(t, lines, "  revoked "+extUnscoped.Ref.String()+" (cached token)")
	assert.Contains(t, lines, "  revoked "+extScoped.Ref.String()+" (cluster token, c-1)")
	assert.Contains(t, lines, "  not revoked "+v3Scoped.Ref.String()+" (v3; recorded on prod)")

	warnings := d.Warnings()
	require.Len(t, warnings, 2)
	assert.True(t, strings.HasPrefix(warnings[0], "the cached token "+v3Failed.Ref.String()+" was not revoked (failed: logout 500; delete 403 Forbidden); "), warnings[0])
	assert.True(t, strings.HasPrefix(warnings[1], "the cluster token "+v3Scoped.Ref.String()+" was not revoked: the CLI cannot delete this v3 token; "), warnings[1])
}
