package token

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLogoutRevokesItsCacheTokensRecordsAndLoginInOrder asserts the main
// path: a logout of an entry holding a login the CLI created and named
// revokes its cache token with the token's own bearer, its same-owner
// superseded record with the stored login, and the login with itself,
// in that order; the entry is left with no TokenRef, a LoggedOutAt stamp
// and no records, and the cache record is gone.
func TestLogoutRevokesItsCacheTokensRecordsAndLoginInOrder(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	old := x.token("alice")
	login := x.login("prod", "alice")
	x.edit(func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		s.SupersededTokens = []config.SupersededToken{{Ref: *old.Ref, UserID: "user-alice", SupersededAt: "2026-10-01T00:00:00Z"}}
		cfg.Servers["prod"] = s
	})
	ct := x.token("alice")
	x.cache("prod-c-1", ct, x.own("prod"), true)
	before := len(x.f.DeleteRequests())

	d, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	assert.Equal(t, []string{ct.Ref.Name, old.Ref.Name, login.Ref.Name}, revokedNames(rep))
	assert.Empty(t, rep.NotRevoked)
	assert.Equal(t, []string{"cache", "superseded", "login"}, []string{rep.Revoked[0].Source, rep.Revoked[1].Source, rep.Revoked[2].Source})
	assert.Equal(t, config.CanonicalServerIdentity(x.url), rep.Revoked[0].Server)
	deletes := x.f.DeleteRequests()[before:]
	require.Len(t, deletes, 3)
	assert.Equal(t, ct.Token, deletes[0].Bearer, "a cache token revokes itself")
	assert.Equal(t, login.Token, deletes[1].Bearer, "a superseded record is revoked with the stored login")
	assert.Equal(t, login.Token, deletes[2].Bearer, "the login revokes itself, last")
	for _, c := range []LoginCredential{old, login, ct} {
		assert.False(t, x.alive(c))
	}
	s := x.load().Servers["prod"]
	assert.Nil(t, s.TokenRef)
	assert.NotEmpty(t, s.LoggedOutAt)
	assert.Equal(t, clierr.ProvenanceDerived, s.Provenance, "provenance is kept")
	assert.Equal(t, "user-alice", s.TokenUserID, "the owner is kept")
	assert.Empty(t, s.SupersededTokens, "confirmed revokes are settled")
	assert.Empty(t, x.cacheKeys())
	assert.Equal(t, []string{"prod-c-1"}, d.DeletedCache)
	assert.Equal(t, []string{"file"}, d.DeletedFrom["prod"])
}

// TestCacheTokenRows asserts the cache-token rows: a record without a ref
// is unproven and never recorded, one whose token does not match its ref is
// noBearer, one expired past the margin is not listed, one with a zero
// expiry is never expired, and a v3 one is recorded and never sent.
func TestCacheTokenRows(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	unnamed, mismatch, expired, zero := x.token("alice"), x.token("alice"), x.token("alice"), x.token("alice")
	x.cache("unnamed", unnamed, x.own("prod"), false)
	b := x.own("prod").WithCreatedToken(&TokenRef{Kind: TokenKindExt, Name: "someone-else"}, "user-alice")
	require.NoError(t, x.store.SetCachedCred("mismatch", mismatch.Token, time.Now().Add(time.Hour), "mismatch", x.url, "c-1", b))
	require.NoError(t, x.store.SetCachedCred("expired", expired.Token, time.Now().Add(-2*time.Hour), "expired", x.url, "c-1", x.own("prod").WithCreatedToken(expired.Ref, "user-alice")))
	require.NoError(t, x.store.SetCachedCred("zero", zero.Token, time.Time{}, "zero", x.url, "c-1", x.own("prod").WithCreatedToken(zero.Ref, "user-alice")))
	v3 := x.own("prod").WithCreatedToken(&TokenRef{Kind: TokenKindV3, Name: "kubeconfig-u-1"}, "user-alice")
	require.NoError(t, x.store.SetCachedCred("v3", "kubeconfig-u-1:secret", time.Now().Add(time.Hour), "v3", x.url, "c-1", v3))
	before := len(x.f.DeleteRequests())

	_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	assert.Contains(t, revokedNames(rep), zero.Ref.Name, "a zero expiry never expires")
	for _, n := range rep.NotRevoked {
		if n.Ref != nil {
			assert.NotEqual(t, expired.Ref.Name, n.Ref.Name, "an expired token is not listed")
		}
	}
	var unproven *NotRevokedToken
	for i, n := range rep.NotRevoked {
		if n.Reason == "unproven" {
			unproven = &rep.NotRevoked[i]
		}
	}
	require.NotNil(t, unproven)
	assert.Nil(t, unproven.Ref)
	assert.Equal(t, "cache", unproven.Source)
	assert.Empty(t, unproven.Entry, "never recorded")
	assert.Equal(t, "manual", unproven.Retry)
	assert.Equal(t, []string{"rancher get tokens"}, unproven.Recovery)

	nb := notRevoked(t, rep, "someone-else")
	assert.Equal(t, "noBearer", nb.Reason)
	assert.Empty(t, nb.Entry, "a damaged record is never recorded")
	assert.Equal(t, "manual", nb.Retry)

	v := notRevoked(t, rep, "kubeconfig-u-1")
	assert.Equal(t, "v3", v.Reason)
	assert.Equal(t, "prod", v.Entry)
	assert.Equal(t, "manual", v.Retry)
	assert.Equal(t, []string{"rancher delete management-token kubeconfig-u-1 --apply --yes --allow-deprecated"}, v.Recovery)

	for _, del := range x.f.DeleteRequests()[before:] {
		assert.NotContains(t, []string{"kubeconfig-u-1", "someone-else", expired.Ref.Name, unnamed.Ref.Name}, del.Name, "nothing is sent for an unrevocable token")
	}
	recs := x.load().Servers["prod"].SupersededTokens
	require.Len(t, recs, 1, "only the v3 token stays recorded")
	assert.Equal(t, "kubeconfig-u-1", recs[0].Ref.Name)
	assert.Equal(t, config.CanonicalServerIdentity(x.url), recs[0].Server)
	assert.Empty(t, x.cacheKeys(), "every record is deleted")
	assert.True(t, x.alive(unnamed))
}

// TestSupersededRecordRows asserts the superseded-record rows: another
// owner, an unknown owner and another server are kept unattempted, and a
// record under a pasted login is noCliBearer.
func TestSupersededRecordRows(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	bobs, unknown, other := x.token("bob"), x.token("alice"), x.token("alice")
	x.edit(func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		s.SupersededTokens = []config.SupersededToken{
			{Ref: *bobs.Ref, UserID: "user-bob", SupersededAt: "2026-10-01T00:00:00Z"},
			{Ref: *unknown.Ref, SupersededAt: "2026-10-01T00:00:00Z"},
			{Ref: *other.Ref, UserID: "user-alice", SupersededAt: "2026-10-01T00:00:00Z", Server: "https://old.ex"},
		}
		cfg.Servers["prod"] = s
	})
	pasted := x.token("alice")
	x.entry("pasted", config.Server{Provenance: clierr.ProvenanceUserSupplied, TokenUserID: "user-alice",
		SupersededTokens: []config.SupersededToken{{Ref: *x.token("alice").Ref, UserID: "user-alice", SupersededAt: "2026-10-01T00:00:00Z"}}})
	require.NoError(t, x.store.SetToken(config.ServerRef{Name: "pasted", URL: x.url}, pasted.Token))

	_, rep, err := x.run(x.req(DeleteForLogout, "prod", "pasted"))
	require.NoError(t, err)
	for _, tc := range []struct{ name, reason, retry, entry string }{
		{bobs.Ref.Name, "otherOwner", "manual", "prod"},
		{unknown.Ref.Name, "unknownOwner", "manual", "prod"},
		{other.Ref.Name, "otherServer", "manual", "prod"},
	} {
		n := notRevoked(t, rep, tc.name)
		assert.Equal(t, tc.reason, n.Reason, tc.name)
		assert.Equal(t, tc.retry, n.Retry, tc.name)
		assert.Equal(t, tc.entry, n.Entry, tc.name)
		assert.NotEmpty(t, n.Recovery, tc.name)
	}
	assert.Equal(t, "rancher auth login 'recover-old.ex' --url 'https://old.ex'   # log in as user 'user-alice'", notRevoked(t, rep, other.Ref.Name).Recovery[0])
	var noCli []NotRevokedToken
	for _, n := range rep.NotRevoked {
		if n.Reason == "noCliBearer" {
			noCli = append(noCli, n)
		}
	}
	require.Len(t, noCli, 1)
	assert.Equal(t, "pasted", noCli[0].Entry)
	assert.Equal(t, "nextLogin", noCli[0].Retry, "the next login the CLI creates for the entry retires it")
	for _, c := range []LoginCredential{bobs, unknown, other} {
		assert.True(t, x.alive(c))
	}
	assert.Len(t, x.load().Servers["prod"].SupersededTokens, 3, "kept records stay")
}

// TestLoginTokenRows asserts the login rows: a pasted login is
// userSupplied with retry none and is never sent; a CLI-created login
// without TokenRef is unproven; a named login whose stored token cannot be
// read is noBearer and still recorded.
func TestLoginTokenRows(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	pasted := x.token("alice")
	x.entry("pasted", config.Server{Provenance: clierr.ProvenanceUserSupplied})
	require.NoError(t, x.store.SetToken(config.ServerRef{Name: "pasted", URL: x.url}, pasted.Token))
	saml := x.token("alice")
	x.entry("saml", config.Server{Provenance: clierr.ProvenanceLegacySession})
	require.NoError(t, x.store.SetToken(config.ServerRef{Name: "saml", URL: x.url}, saml.Token))
	unread := x.token("alice")
	x.entry("unread", config.Server{Provenance: clierr.ProvenanceDerived, TokenRef: unread.Ref, TokenUserID: "user-alice"})
	before := len(x.f.DeleteRequests())

	_, rep, err := x.run(x.req(DeleteForLogout, "pasted", "saml", "unread"))
	require.NoError(t, err)
	assert.Empty(t, rep.Revoked)
	byReason := map[string]NotRevokedToken{}
	for _, n := range rep.NotRevoked {
		byReason[n.Reason] = n
	}
	assert.Equal(t, "none", byReason["userSupplied"].Retry)
	assert.Empty(t, byReason["userSupplied"].Recovery)
	assert.Equal(t, "login", byReason["unproven"].Source)
	assert.Equal(t, "manual", byReason["unproven"].Retry)
	nb := notRevoked(t, rep, unread.Ref.Name)
	assert.Equal(t, "noBearer", nb.Reason)
	assert.Equal(t, "unread", nb.Entry)
	assert.Equal(t, "nextLogin", nb.Retry)
	assert.Len(t, x.f.DeleteRequests(), before, "nothing is sent")
	recs := x.load().Servers["unread"].SupersededTokens
	require.Len(t, recs, 1, "a named login is recorded whatever its bearer")
	assert.Equal(t, unread.Ref.Name, recs[0].Ref.Name)
	assert.True(t, x.alive(pasted))
	assert.True(t, x.alive(saml))
}

// TestAnEntryLoggedOutByAnEarlierVersion asserts the entry an earlier
// logout left (LoggedOutAt and TokenRef both set): its token is recorded,
// nothing is sent, it is reported noBearer with its entry, and TokenRef is
// cleared.
func TestAnEntryLoggedOutByAnEarlierVersion(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	kept := x.token("alice")
	x.entry("prod", config.Server{Provenance: clierr.ProvenanceDerived, TokenRef: kept.Ref, TokenUserID: "user-alice", LoggedOutAt: "2026-09-01T00:00:00Z"})
	before := len(x.f.DeleteRequests())

	_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	n := notRevoked(t, rep, kept.Ref.Name)
	assert.Equal(t, "noBearer", n.Reason)
	assert.Equal(t, "prod", n.Entry)
	assert.Equal(t, "nextLogin", n.Retry)
	assert.Len(t, x.f.DeleteRequests(), before)
	s := x.load().Servers["prod"]
	assert.Nil(t, s.TokenRef)
	require.Len(t, s.SupersededTokens, 1)
	assert.Equal(t, kept.Ref.Name, s.SupersededTokens[0].Ref.Name)
}

// TestEachAnswerToARevoke asserts how each answer reads: to a token's own
// bearer 401 and 404 are unconfirmed and 403 forbidden, whose record the
// write after the revokes removes; 500 is failed with the status; to the stored login 404 is a
// confirmed revoke and 403 is failed. A recorded failure is stamped.
func TestEachAnswerToARevoke(t *testing.T) {
	for _, tc := range []struct {
		name, mode, reason string
		viaLogin           bool
		revoked, recorded  bool
	}{
		{"own bearer, 401", "unauthorized", "unconfirmed", false, false, true},
		{"own bearer, 404", "gone", "unconfirmed", false, false, true},
		{"own bearer, 403", "forbidden", "forbidden", false, false, false},
		{"own bearer, 500", "fail", "failed", false, false, true},
		{"stored login, 404", "gone", "", true, true, false},
		{"stored login, 403", "forbidden", "failed", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newRevFixture(t, testrancher.Options{})
			target := x.token("bob")
			x.login("prod", "bob")
			if tc.viaLogin {
				x.edit(func(cfg *config.Config) {
					s := cfg.Servers["prod"]
					s.SupersededTokens = []config.SupersededToken{{Ref: *target.Ref, UserID: "user-bob", SupersededAt: "2026-10-01T00:00:00Z"}}
					cfg.Servers["prod"] = s
				})
			} else {
				// The record's owner is not the login's: no retry with it.
				x.cache("k", target, x.own("prod").WithCreatedToken(target.Ref, "user-other"), false)
			}
			x.f.SetDeleteMode(target.Ref.Name, tc.mode)

			_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
			require.NoError(t, err)
			if tc.revoked {
				assert.Contains(t, revokedNames(rep), target.Ref.Name)
				assert.Empty(t, x.load().Servers["prod"].SupersededTokens)
				return
			}
			n := notRevoked(t, rep, target.Ref.Name)
			assert.Equal(t, tc.reason, n.Reason)
			if tc.reason == "failed" {
				assert.NotEmpty(t, n.Error)
			}
			recs := x.load().Servers["prod"].SupersededTokens
			if !tc.recorded {
				assert.Empty(t, recs)
				assert.Empty(t, n.Entry)
				assert.Equal(t, "manual", n.Retry)
				return
			}
			require.Len(t, recs, 1)
			require.NotNil(t, recs[0].LastAttempt, "an attempted record is stamped")
			assert.Equal(t, "prod", n.Entry)
		})
	}
}

// TestAnUnconfirmedCacheTokenIsRetriedWithTheLogin asserts the retry: a
// cache token whose own bearer gets 401 (it was deleted already) is
// retried with the stored login of the same server and owner, whose 404
// confirms it gone; a 401 to the login too leaves it unconfirmed; a token
// of another or an unknown owner gets no retry.
func TestAnUnconfirmedCacheTokenIsRetriedWithTheLogin(t *testing.T) {
	t.Run("the login confirms it gone", func(t *testing.T) {
		x := newRevFixture(t, testrancher.Options{})
		login := x.login("prod", "alice")
		ct := x.token("alice")
		x.cache("k", ct, x.own("prod"), true)
		x.f.SetDeleteMode(ct.Ref.Name, "gone")
		before := len(x.f.DeleteRequests())
		_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		assert.Equal(t, []string{ct.Ref.Name, login.Ref.Name}, revokedNames(rep))
		deletes := x.f.DeleteRequests()[before:]
		require.Len(t, deletes, 3)
		assert.Equal(t, ct.Token, deletes[0].Bearer)
		assert.Equal(t, login.Token, deletes[1].Bearer, "the retry uses the login, before the login is revoked")
	})
	t.Run("a 401 to the login leaves it unconfirmed", func(t *testing.T) {
		x := newRevFixture(t, testrancher.Options{})
		x.login("prod", "alice")
		ct := x.token("alice")
		x.cache("k", ct, x.own("prod"), true)
		x.f.SetDeleteMode(ct.Ref.Name, "unauthorized")
		_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		assert.Equal(t, "unconfirmed", notRevoked(t, rep, ct.Ref.Name).Reason)
	})
	for _, owner := range []string{"user-bob", ""} {
		t.Run("owner "+owner, func(t *testing.T) {
			x := newRevFixture(t, testrancher.Options{})
			x.login("prod", "alice")
			ct := x.token("alice")
			x.cache("k", ct, x.own("prod").WithCreatedToken(ct.Ref, owner), false)
			x.f.SetDeleteMode(ct.Ref.Name, "gone")
			before := len(x.f.DeleteRequests())
			_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
			require.NoError(t, err)
			n := notRevoked(t, rep, ct.Ref.Name)
			assert.Equal(t, "unconfirmed", n.Reason)
			assert.Equal(t, "manual", n.Retry, "an owner the entry does not record is never retried by its login")
			assert.Len(t, x.f.DeleteRequests()[before:], 2, "no retry: the cache token and the login")
		})
	}
}

// TestNoRetryWithTheLoginOnceTheBudgetHasPassed asserts that an
// unconfirmed cache token is not retried with the login when the budget's
// deadline passed during its own request: it stays unconfirmed, and the
// login after it is not sent either.
func TestNoRetryWithTheLoginOnceTheBudgetHasPassed(t *testing.T) {
	var armed, late atomic.Bool
	old := revocationClock
	revocationClock = func() time.Time {
		if late.Load() {
			return time.Now().Add(time.Hour)
		}
		return time.Now()
	}
	t.Cleanup(func() { revocationClock = old })
	x := newRevFixture(t, testrancher.Options{RequestHook: func(r *http.Request) {
		if r.Method == http.MethodDelete && armed.Load() {
			late.Store(true)
		}
	}})
	login := x.login("prod", "alice")
	ct := x.token("alice")
	x.cache("k", ct, x.own("prod"), true)
	x.f.SetDeleteMode(ct.Ref.Name, "gone")
	before := len(x.f.DeleteRequests())
	armed.Store(true)
	_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	assert.Equal(t, "unconfirmed", notRevoked(t, rep, ct.Ref.Name).Reason)
	assert.Equal(t, "budget", notRevoked(t, rep, login.Ref.Name).Reason)
	deletes := x.f.DeleteRequests()[before:]
	require.Len(t, deletes, 1)
	assert.Equal(t, ct.Token, deletes[0].Bearer)
}

// TestALogoutLeavesRecordsOtherEntriesRead asserts the narrowed scope: a
// record another entry still reads stays, a record of a removed entry on
// the target's server is deleted and noEntry with nothing sent, and a
// record with no server stays.
func TestALogoutLeavesRecordsOtherEntriesRead(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	x.login("dev", "bob")
	devs, gone, bare := x.token("bob"), x.token("alice"), x.token("alice")
	x.cache("dev-own", devs, x.own("dev"), true)
	x.entry("removed", config.Server{})
	x.cache("removed-own", gone, x.own("removed"), true)
	x.edit(func(cfg *config.Config) { delete(cfg.Servers, "removed") })
	require.NoError(t, x.store.SetCachedCred("bare", bare.Token, time.Now().Add(time.Hour), "bare", "", "", config.CacheBindingForTest(config.CanonicalDir(x.dir), "", "")))
	before := len(x.f.DeleteRequests())

	_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"dev-own", "bare"}, x.cacheKeys())
	n := notRevoked(t, rep, gone.Ref.Name)
	assert.Equal(t, "noEntry", n.Reason)
	assert.Empty(t, n.Entry)
	assert.Equal(t, "manual", n.Retry)
	for _, del := range x.f.DeleteRequests()[before:] {
		assert.NotEqual(t, gone.Ref.Name, del.Name)
		assert.NotEqual(t, devs.Ref.Name, del.Name)
	}
	assert.True(t, x.alive(devs))
}

// TestRecordsAreWrittenBeforeTheDeletes asserts that when a login delete
// runs, the config already records every token the delete removes, and a
// record written between the two holds survives; one rewritten there is
// kept and its old token, no longer cached, is revoked.
func TestRecordsAreWrittenBeforeTheDeletes(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	login := x.login("prod", "alice")
	ct, rewritten, fresh, later := x.token("alice"), x.token("alice"), x.token("alice"), x.token("alice")
	x.cache("ct", ct, x.own("prod"), true)
	x.cache("rewritten", rewritten, x.own("prod"), true)
	req := x.req(DeleteForLogout, "prod")
	req.DeleteLogin = func(ctx context.Context, ref config.ServerRef) ([]string, []string, error) {
		var names []string
		for _, r := range x.load().Servers["prod"].SupersededTokens {
			names = append(names, r.Ref.Name)
		}
		assert.ElementsMatch(t, []string{ct.Ref.Name, rewritten.Ref.Name, login.Ref.Name}, names, "every token is recorded before a delete")
		require.NoError(t, x.store.SetCachedCred("rewritten", fresh.Token, time.Now().Add(time.Hour), "rewritten", x.url, "c-1", config.CacheBindingForTest(config.CanonicalDir(x.dir), "", "")))
		require.NoError(t, x.store.SetCachedCred("later", later.Token, time.Now().Add(time.Hour), "later", x.url, "c-1", config.CacheBindingForTest(config.CanonicalDir(x.dir), "", "")))
		return x.deleteLogin(ctx, ref)
	}
	d, rep, err := x.run(req)
	require.NoError(t, err)
	assert.Equal(t, []string{"rewritten"}, d.Kept)
	assert.ElementsMatch(t, []string{"rewritten", "later"}, x.cacheKeys())
	assert.Contains(t, revokedNames(rep), rewritten.Ref.Name)
	assert.True(t, x.alive(fresh))
	assert.True(t, x.alive(later))
}

// TestSettleTouchesOnlyAttemptedRecords asserts that a login made while
// the revokes run keeps its token and its records.
func TestSettleTouchesOnlyAttemptedRecords(t *testing.T) {
	var x *revFixture
	var armed atomic.Bool
	var newLogin LoginCredential
	x = newRevFixture(t, testrancher.Options{RequestHook: func(r *http.Request) {
		// Armed for the first revoke only: the fixture's own logins send
		// deletes too.
		if r.Method != http.MethodDelete || !armed.CompareAndSwap(true, false) {
			return
		}
		x.edit(func(cfg *config.Config) {
			s := cfg.Servers["prod"]
			s.TokenRef, s.LoggedOutAt = newLogin.Ref, ""
			s.SupersededTokens = append(s.SupersededTokens, config.SupersededToken{Ref: TokenRef{Kind: TokenKindExt, Name: "token-of-the-new-login"}, UserID: "user-alice", SupersededAt: "2026-10-06T00:00:00Z"})
			cfg.Servers["prod"] = s
		})
	}})
	newLogin = x.token("alice")
	x.login("prod", "alice")
	ct := x.token("alice")
	x.cache("ct", ct, x.own("prod"), true)
	x.f.SetDeleteMode(ct.Ref.Name, "fail")
	d, err := x.localDelete(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	armed.Store(true)
	d.RevokeTokens(t.Context())
	require.Empty(t, d.Settle(t.Context()))
	s := x.load().Servers["prod"]
	assert.Equal(t, newLogin.Ref, s.TokenRef)
	var names []string
	for _, r := range s.SupersededTokens {
		names = append(names, r.Ref.Name)
		if r.Ref.Name == "token-of-the-new-login" {
			assert.Nil(t, r.LastAttempt)
		}
	}
	assert.ElementsMatch(t, []string{ct.Ref.Name, "token-of-the-new-login"}, names)
}

// TestNoRequestRunsUnderTheConfigLock asserts that every revoke runs after
// the local delete's lock span.
func TestNoRequestRunsUnderTheConfigLock(t *testing.T) {
	var violations atomic.Int32
	var dir string
	x := newRevFixture(t, testrancher.Options{RequestHook: func(r *http.Request) {
		if r.Method == http.MethodDelete && configLockHeld(dir) {
			violations.Add(1)
		}
	}})
	dir = x.dir
	x.login("prod", "alice")
	x.cache("ct", x.token("alice"), x.own("prod"), true)
	_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	assert.Len(t, rep.Revoked, 2)
	assert.Equal(t, int32(0), violations.Load())
}

// TestAFailedFirstWriteDeletesNothing asserts that when the first config
// write fails nothing is deleted and no Deletion is returned.
func TestAFailedFirstWriteDeletesNothing(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	login := x.login("prod", "alice")
	x.cache("ct", x.token("alice"), x.own("prod"), true)
	req := x.req(DeleteForLogout, "prod")
	req.Cache = failingWriteStore{CredStore: x.store, dir: x.dir}
	d, err := x.localDelete(req)
	require.Error(t, err)
	assert.Nil(t, d)
	assert.Equal(t, []string{"ct"}, x.cacheKeys())
	tok, terr := x.store.GetToken(config.ServerRef{Name: "prod", URL: x.url})
	require.NoError(t, terr)
	assert.Equal(t, login.Token, tok)
	assert.Equal(t, login.Ref, x.load().Servers["prod"].TokenRef)
}

// failingWriteStore makes the config directory read-only inside the first
// hold, so the first write fails.
type failingWriteStore struct {
	config.CredStore
	dir string
}

func (s failingWriteStore) HoldCachedCreds(fn func([]config.CachedRecord) error) error {
	return s.CredStore.HoldCachedCreds(func(recs []config.CachedRecord) error {
		if err := os.Chmod(s.dir, 0o500); err != nil {
			return err
		}
		defer func() { _ = os.Chmod(s.dir, 0o700) }()
		return fn(recs)
	})
}

// TestAFailedLoginDeleteRestoresTheEntry asserts the failure path: the
// target's TokenRef and LoggedOutAt are put back and the record written
// for its login removed, its login is notDeleted and never sent, the
// planned cache records are still deleted and their tokens revoked, and
// the error names the target.
func TestAFailedLoginDeleteRestoresTheEntry(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	login := x.login("prod", "alice")
	ct := x.token("alice")
	x.cache("ct", ct, x.own("prod"), true)
	req := x.req(DeleteForLogout, "prod")
	req.DeleteLogin = func(context.Context, config.ServerRef) ([]string, []string, error) {
		return nil, nil, errors.New("the helper refused")
	}
	_, rep, err := x.run(req)
	var lde *LoginDeleteError
	require.ErrorAs(t, err, &lde)
	assert.Equal(t, "prod", lde.Target)
	assert.Equal(t, []string{ct.Ref.Name}, revokedNames(rep))
	n := notRevoked(t, rep, login.Ref.Name)
	assert.Equal(t, "notDeleted", n.Reason)
	assert.Empty(t, n.Entry, "the restore removed its record")
	assert.Equal(t, "manual", n.Retry)
	assert.Equal(t, []string{"rancher auth logout"}, n.Recovery)
	assert.True(t, x.alive(login))
	s := x.load().Servers["prod"]
	assert.Equal(t, login.Ref, s.TokenRef)
	assert.Empty(t, s.LoggedOutAt)
	assert.Empty(t, s.SupersededTokens)
	assert.Empty(t, x.cacheKeys())
}

// TestBudgetAndCancellation asserts that tokens not reached within the
// budget are reported budget, which is not an interrupt, that a cancelled
// context stops before the next revoke with the rest cancelled, and that
// the record write after them still runs.
func TestBudgetAndCancellation(t *testing.T) {
	t.Run("budget", func(t *testing.T) {
		old := revocationBudget
		revocationBudget = 300 * time.Millisecond
		t.Cleanup(func() { revocationBudget = old })
		x := newRevFixture(t, testrancher.Options{})
		x.login("prod", "alice")
		ct := x.token("alice")
		x.cache("ct", ct, x.own("prod"), true)
		x.f.SetDeleteMode(ct.Ref.Name, "hang")
		d, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		assert.False(t, d.Cancelled(), "a run that only ran out of budget was not interrupted")
		assert.Equal(t, "failed", notRevoked(t, rep, ct.Ref.Name).Reason, "the hanging request spends the budget")
		var reasons []string
		for _, n := range rep.NotRevoked {
			reasons = append(reasons, n.Reason)
		}
		assert.Contains(t, reasons, "budget", "the login is not reached")
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		var armed atomic.Bool
		x := newRevFixture(t, testrancher.Options{RequestHook: func(r *http.Request) {
			if r.Method == http.MethodDelete && armed.CompareAndSwap(true, false) {
				cancel()
			}
		}})
		x.login("prod", "alice")
		ct := x.token("alice")
		x.cache("ct", ct, x.own("prod"), true)
		d, err := x.localDelete(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		armed.Store(true)
		d.RevokeTokens(ctx)
		require.Empty(t, d.Settle(ctx))
		rep := d.Report()
		assert.Equal(t, []string{ct.Ref.Name}, revokedNames(rep), "the request in flight completes")
		require.Len(t, rep.NotRevoked, 1)
		assert.Equal(t, "cancelled", rep.NotRevoked[0].Reason)
		assert.True(t, d.Cancelled())
		recs := x.load().Servers["prod"].SupersededTokens
		require.Len(t, recs, 1, "the confirmed revoke is settled, the login's record stays")
	})
}
