package token

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTheMatchingBearerRuleStaysOnItsEntry asserts that a superseded
// record whose ref is the stored login token is revoked with that token as
// its own bearer only on that login's own CLI-created entry and server: on
// a pasted login it is noCliBearer, on another entry it follows that
// entry's row, and with another server it is otherServer.
func TestTheMatchingBearerRuleStaysOnItsEntry(t *testing.T) {
	setup := func(t *testing.T, provenance clierr.Provenance, recordServer string) (*revFixture, LoginCredential) {
		t.Helper()
		x := newRevFixture(t, testrancher.Options{})
		login := x.token("alice")
		// The state a failed restore write leaves: no TokenRef, stamped,
		// the login still stored, its record on the entry.
		x.entry("prod", config.Server{Provenance: provenance, TokenUserID: "user-alice", LoggedOutAt: "2026-10-06T00:00:00Z",
			SupersededTokens: []config.SupersededToken{{Ref: *login.Ref, UserID: "user-alice", SupersededAt: "2026-10-06T00:00:00Z", Server: recordServer}}})
		require.NoError(t, x.store.SetToken(config.ServerRef{Name: "prod", URL: x.url}, login.Token))
		return x, login
	}
	t.Run("its own CLI-created entry", func(t *testing.T) {
		x, login := setup(t, clierr.ProvenanceDerived, "")
		_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		assert.Equal(t, []string{login.Ref.Name}, revokedNames(rep))
	})
	t.Run("a pasted login", func(t *testing.T) {
		x, login := setup(t, clierr.ProvenanceUserSupplied, "")
		before := len(x.f.DeleteRequests())
		_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		assert.Equal(t, "noCliBearer", notRevoked(t, rep, login.Ref.Name).Reason)
		assert.Len(t, x.f.DeleteRequests(), before)
	})
	t.Run("another server", func(t *testing.T) {
		x, login := setup(t, clierr.ProvenanceDerived, "https://old.ex")
		_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		assert.Equal(t, "otherServer", notRevoked(t, rep, login.Ref.Name).Reason)
	})
	t.Run("another entry", func(t *testing.T) {
		x, login := setup(t, clierr.ProvenanceDerived, "")
		x.edit(func(cfg *config.Config) {
			s := cfg.Servers["prod"]
			cfg.Servers["dev"] = config.Server{URL: x.url, Provenance: clierr.ProvenanceDerived, TokenUserID: "user-alice", SupersededTokens: s.SupersededTokens}
			s.SupersededTokens = nil
			cfg.Servers["prod"] = s
		})
		_, rep, err := x.run(x.req(DeleteForLogout, "prod", "dev"))
		require.NoError(t, err)
		n := notRevoked(t, rep, login.Ref.Name)
		assert.Equal(t, "noBearer", n.Reason, "dev reads no login of its own to bear the record")
		assert.Equal(t, "dev", n.Entry)
	})
}

// TestAV3SupersededRecordIsNeverSent asserts that a v3 record on the entry
// is kept, reported v3 with the administrator's delete, and never sent.
func TestAV3SupersededRecordIsNeverSent(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	x.edit(func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		s.SupersededTokens = []config.SupersededToken{{Ref: TokenRef{Kind: TokenKindV3, Name: "kubeconfig-u-1"}, UserID: "user-alice", SupersededAt: "2026-10-01T00:00:00Z"}}
		cfg.Servers["prod"] = s
	})
	before := len(x.f.DeleteRequests())
	_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	n := notRevoked(t, rep, "kubeconfig-u-1")
	assert.Equal(t, "v3", n.Reason)
	assert.Equal(t, "manual", n.Retry)
	for _, d := range x.f.DeleteRequests()[before:] {
		assert.NotEqual(t, "kubeconfig-u-1", d.Name)
	}
	recs := x.load().Servers["prod"].SupersededTokens
	require.Len(t, recs, 1, "the v3 record stays; the revoked login's record is settled")
	assert.Equal(t, "kubeconfig-u-1", recs[0].Ref.Name)
}

// TestAServerWideRecordGoesToAnEntryWithNoOwner asserts that an entry
// whose owner is not known (a SAML legacy login) still takes a
// server-wide record of its server: recorded there and revoked with the
// token's own bearer, never noEntry.
func TestAServerWideRecordGoesToAnEntryWithNoOwner(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.entry("saml", config.Server{Provenance: clierr.ProvenanceLegacySession})
	ct := x.token("alice")
	x.cache("k", ct, x.serverWide(), true)
	_, rep, err := x.run(x.req(DeleteForLogout, "saml"))
	require.NoError(t, err)
	assert.Equal(t, []string{ct.Ref.Name}, revokedNames(rep))
}

// TestDeleteTokenNeverRetriesWithALogin asserts that auth delete-token,
// which reads no login, leaves an unconfirmed cache token unconfirmed.
func TestDeleteTokenNeverRetriesWithALogin(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	ct := x.token("alice")
	x.cache("k", ct, x.own("prod"), true)
	x.f.SetDeleteMode(ct.Ref.Name, "gone")
	req := x.req(DeleteForDeleteToken)
	req.Select = func(config.CachedRecord) bool { return true }
	req.DeleteLogin = func(context.Context, config.ServerRef) ([]string, []string, error) {
		return nil, nil, errors.New("delete-token deletes no login")
	}
	before := len(x.f.DeleteRequests())
	_, rep, err := x.run(req)
	require.NoError(t, err)
	assert.Equal(t, "unconfirmed", notRevoked(t, rep, ct.Ref.Name).Reason)
	assert.Len(t, x.f.DeleteRequests()[before:], 1)
}

// TestWhereAPluginAnswerIsRecorded asserts the placement of a record a
// plugin login entry answered for a call that identified an entry: on the
// identified entry when it is itself a listed plugin login entry, else on
// the first listed entry that is still a plugin login entry.
func TestWhereAPluginAnswerIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		demote []string
		want   string
	}{
		{"the identified entry is a listed plugin login entry", nil, "exec-login-c"},
		{"the first listed entry that is still a plugin login entry", []string{"exec-login-a", "exec-login-c"}, "exec-login-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newRevFixture(t, testrancher.Options{})
			x.edit(func(cfg *config.Config) { cfg.TokenReuse = true })
			for _, n := range []string{"exec-login-a", "exec-login-b", "exec-login-c"} {
				x.entry(n, config.Server{Provenance: clierr.ProvenanceExecLogin, TokenUserID: "user-alice"})
			}
			ct := x.token("alice")
			x.f.SetDeleteMode(ct.Ref.Name, "fail")
			x.cache("k", ct, config.CacheBindingForIdentified(config.CanonicalDir(x.dir), x.load(), x.url, "exec-login-c", "", ""), true)
			x.edit(func(cfg *config.Config) {
				for _, n := range tc.demote {
					s := cfg.Servers[n]
					s.Provenance = clierr.ProvenanceDerived
					cfg.Servers[n] = s
				}
			})
			req := x.req(DeleteForDeleteToken)
			req.Select = func(config.CachedRecord) bool { return true }
			_, rep, err := x.run(req)
			require.NoError(t, err)
			assert.Equal(t, tc.want, notRevoked(t, rep, ct.Ref.Name).Entry)
		})
	}
}

// TestUpdateServerFallsBackToTheMovedEntry asserts that a server-wide
// record with no other entry naming the old server is recorded on the
// moved entry, otherServer, never noEntry.
func TestUpdateServerFallsBackToTheMovedEntry(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	ct := x.token("alice")
	x.cache("wide", ct, x.serverWide(), true)
	d, err := x.localDelete(x.req(DeleteForUpdateServer, "prod"))
	require.NoError(t, err)
	n := notRevoked(t, d.Report(), ct.Ref.Name)
	assert.Equal(t, "otherServer", n.Reason)
	assert.Equal(t, "prod", n.Entry)
}

// TestTheParentEntrysTrustComesFirst asserts that a record's own entry,
// when it names the record's server, is the trust used, before any other
// entry for the server with a CA fingerprint.
func TestTheParentEntrysTrustComesFirst(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	tlsF := testrancher.New(testrancher.Options{Users: revocationUsers})
	tlsSrv := httptest.NewTLSServer(tlsF)
	t.Cleanup(tlsSrv.Close)
	// A certificate of its own: every httptest TLS server presents the
	// same built-in one, which would make any CA right.
	_, wrongCA, err := testrancher.LoopbackTLSCertificate()
	require.NoError(t, err)
	ct := tokenOn(t, tlsSrv, "alice")
	x.entry("prod", config.Server{URL: tlsSrv.URL, CA: certPEM(t, tlsSrv)})
	x.entry("aaa", config.Server{URL: tlsSrv.URL, CA: string(wrongCA), CAFingerprint: "ab12"})
	b := config.CacheBindingFor(config.CanonicalDir(x.dir), x.load(), "prod").WithCreatedToken(ct.Ref, "user-alice")
	require.NoError(t, x.store.SetCachedCred("k", ct.Token, time.Now().Add(time.Hour), "k", config.CanonicalServerIdentity(tlsSrv.URL), "c-1", b))
	req := x.req(DeleteForDeleteToken)
	req.Select = func(config.CachedRecord) bool { return true }
	_, rep, rerr := x.run(req)
	require.NoError(t, rerr)
	assert.Equal(t, []string{ct.Ref.Name}, revokedNames(rep))
}

// TestALoginTokenAlsoRecordedIsRevokedOnce asserts that a superseded record
// naming the entry's own current token is left to the login row: one
// DELETE, one revoked item.
func TestALoginTokenAlsoRecordedIsRevokedOnce(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	login := x.login("prod", "alice")
	x.edit(func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		s.SupersededTokens = []config.SupersededToken{{Ref: *login.Ref, UserID: "user-alice", SupersededAt: "2026-10-01T00:00:00Z"}}
		cfg.Servers["prod"] = s
	})
	before := len(x.f.DeleteRequests())
	_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	assert.Equal(t, []string{login.Ref.Name}, revokedNames(rep))
	assert.Empty(t, rep.NotRevoked)
	assert.Len(t, x.f.DeleteRequests()[before:], 1)
}

// TestATokenAlreadyRecordedIsNotRecordedTwice asserts that the first write
// adds no second record for a token its entry already records.
func TestATokenAlreadyRecordedIsNotRecordedTwice(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	login := x.login("prod", "alice")
	x.edit(func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		s.SupersededTokens = []config.SupersededToken{{Ref: *login.Ref, UserID: "user-alice", SupersededAt: "2026-10-01T00:00:00Z"}}
		cfg.Servers["prod"] = s
	})
	x.f.SetDeleteMode(login.Ref.Name, "fail")
	_, _, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	recs := x.load().Servers["prod"].SupersededTokens
	require.Len(t, recs, 1)
	assert.Equal(t, login.Ref.Name, recs[0].Ref.Name)
}

// TestAnEntryWithNoLoginReportsNothing asserts that logging out an entry
// that never held a login lists no token.
func TestAnEntryWithNoLoginReportsNothing(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.entry("plain", config.Server{})
	x.entry("saml", config.Server{Provenance: clierr.ProvenanceLegacySession})
	_, rep, err := x.run(x.req(DeleteForLogout, "plain", "saml"))
	require.NoError(t, err)
	assert.Empty(t, rep.Revoked)
	assert.Empty(t, rep.NotRevoked)
}

// TestAFailedSecondHoldStillRevokesUnderLogoutAndDeleteServer asserts that
// under auth logout (one entry or --all) and config delete-server a cache
// token whose record the second hold could not delete is revoked as
// planned - the binding advance already made the record a miss - and, when
// its revoke fails, stays recorded with retry by the owner rule (the entry
// stays: delete-server removes it only after a clean run). The local
// delete's error is still returned.
func TestAFailedSecondHoldStillRevokesUnderLogoutAndDeleteServer(t *testing.T) {
	for _, cmd := range []DeleteCommand{DeleteForLogout, DeleteForLogoutAll, DeleteForDeleteServer} {
		for _, fail := range []bool{false, true} {
			x := newRevFixture(t, testrancher.Options{})
			x.login("prod", "alice")
			ct := x.token("alice")
			x.cache("ct", ct, x.own("prod"), true)
			if fail {
				x.f.SetDeleteMode(ct.Ref.Name, "fail")
			}
			req := x.req(cmd, "prod")
			req.Cache = failingSecondHold{CredStore: x.store}
			_, rep, err := x.run(req)
			require.ErrorContains(t, err, "exec-credential cache")
			if !fail {
				assert.Contains(t, revokedNames(rep), ct.Ref.Name)
				assert.False(t, x.alive(ct))
				continue
			}
			n := notRevoked(t, rep, ct.Ref.Name)
			assert.Equal(t, "failed", n.Reason)
			assert.Equal(t, "prod", n.Entry)
			assert.Equal(t, "nextLogin", n.Retry)
		}
	}
}

// TestTheLoginRetryNeedsKnownOwnersAndAMatchingLogin asserts the two
// guards of the unconfirmed-cache retry: no retry when neither the record
// nor the entry names an owner, and none with a stored login that does not
// match the token its entry records.
func TestTheLoginRetryNeedsKnownOwnersAndAMatchingLogin(t *testing.T) {
	t.Run("no owner on either side", func(t *testing.T) {
		x := newRevFixture(t, testrancher.Options{})
		login := x.token("alice")
		x.entry("prod", config.Server{Provenance: clierr.ProvenanceDerived, TokenRef: login.Ref})
		require.NoError(t, x.store.SetToken(config.ServerRef{Name: "prod", URL: x.url}, login.Token))
		ct := x.token("alice")
		x.cache("ct", ct, x.own("prod").WithCreatedToken(ct.Ref, ""), false)
		x.f.SetDeleteMode(ct.Ref.Name, "gone")
		before := len(x.f.DeleteRequests())
		_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		assert.Equal(t, "unconfirmed", notRevoked(t, rep, ct.Ref.Name).Reason)
		assert.Len(t, x.f.DeleteRequests()[before:], 2, "the cache token and the login, no retry")
	})
	t.Run("a stored login that is not the recorded token", func(t *testing.T) {
		x := newRevFixture(t, testrancher.Options{})
		login, recorded := x.token("alice"), x.token("alice")
		x.entry("prod", config.Server{Provenance: clierr.ProvenanceDerived, TokenRef: recorded.Ref, TokenUserID: "user-alice"})
		require.NoError(t, x.store.SetToken(config.ServerRef{Name: "prod", URL: x.url}, login.Token))
		ct := x.token("alice")
		x.cache("ct", ct, x.own("prod"), true)
		x.f.SetDeleteMode(ct.Ref.Name, "gone")
		_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		assert.Equal(t, "unconfirmed", notRevoked(t, rep, ct.Ref.Name).Reason)
		assert.Equal(t, "noBearer", notRevoked(t, rep, recorded.Ref.Name).Reason)
	})
}

// TestAnEmptyOwnerNeverMatchesInPlacement asserts that a server-wide record
// with no owner is not drawn to a candidate entry with no owner: the first
// candidate by name takes it.
func TestAnEmptyOwnerNeverMatchesInPlacement(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	x.entry("aaa", config.Server{TokenUserID: "user-x"})
	x.entry("bbb", config.Server{})
	ct := x.token("alice")
	x.f.SetDeleteMode(ct.Ref.Name, "fail")
	x.cache("wide", ct, x.serverWide().WithCreatedToken(ct.Ref, ""), false)
	_, rep, err := x.run(x.req(DeleteForDeleteServer, "prod"))
	require.NoError(t, err)
	assert.Equal(t, "aaa", notRevoked(t, rep, ct.Ref.Name).Entry)
}

// TestACAFingerprintComesBeforeACA asserts the trust order among the
// entries that name a record's server when its own entry does not: the one
// with a CA fingerprint before one with a CA alone.
func TestACAFingerprintComesBeforeACA(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	tlsF := testrancher.New(testrancher.Options{Users: revocationUsers})
	tlsSrv := httptest.NewTLSServer(tlsF)
	t.Cleanup(tlsSrv.Close)
	_, wrongCA, err := testrancher.LoopbackTLSCertificate()
	require.NoError(t, err)
	ct := tokenOn(t, tlsSrv, "alice")
	x.entry("prod", config.Server{URL: tlsSrv.URL})
	b := config.CacheBindingFor(config.CanonicalDir(x.dir), x.load(), "prod").WithCreatedToken(ct.Ref, "user-alice")
	require.NoError(t, x.store.SetCachedCred("k", ct.Token, time.Now().Add(time.Hour), "k", config.CanonicalServerIdentity(tlsSrv.URL), "c-1", b))
	x.edit(func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		s.URL = "https://elsewhere.ex"
		cfg.Servers["prod"] = s
		cfg.Servers["a-ca"] = config.Server{URL: tlsSrv.URL, CA: string(wrongCA)}
		cfg.Servers["b-fingerprint"] = config.Server{URL: tlsSrv.URL, CA: certPEM(t, tlsSrv), CAFingerprint: "ab12"}
	})
	req := x.req(DeleteForDeleteToken)
	req.Select = func(config.CachedRecord) bool { return true }
	_, rep, rerr := x.run(req)
	require.NoError(t, rerr)
	assert.Equal(t, []string{ct.Ref.Name}, revokedNames(rep))
}

// TestAnEarlierLogoutsTokenStaysNoBearerWhenTheDeleteFails asserts that the
// token an earlier version's logout left named (no login stored) is
// reported noBearer when the login delete fails: notDeleted is for a login
// still stored and in use.
func TestAnEarlierLogoutsTokenStaysNoBearerWhenTheDeleteFails(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	kept := x.token("alice")
	x.entry("prod", config.Server{Provenance: clierr.ProvenanceDerived, TokenRef: kept.Ref, TokenUserID: "user-alice", LoggedOutAt: "2026-09-01T00:00:00Z"})
	req := x.req(DeleteForLogout, "prod")
	req.DeleteLogin = func(context.Context, config.ServerRef) ([]string, []string, error) {
		return nil, nil, errors.New("the keychain refused")
	}
	_, rep, err := x.run(req)
	require.Error(t, err)
	assert.Equal(t, "noBearer", notRevoked(t, rep, kept.Ref.Name).Reason)
}

// TestALoginDeleteThatFailedCountsAsDeletedOnlyWhenTheTokenIsGone asserts
// that a failed login delete stays the command's failure and stops the
// login deletes after it, and counts the login as deleted only when the
// store it was read from no longer holds the token: then it is revoked,
// its TokenRef stays cleared and its backends are listed, though it is not
// among the deleted targets, and the read's own notices are kept. A token
// still stored, or a store that cannot be read, leaves the login
// notDeleted and its TokenRef put back. The next target is never
// deleted: its login stays stored and in use.
func TestALoginDeleteThatFailedCountsAsDeletedOnlyWhenTheTokenIsGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		// step runs in the failed delete, before it returns.
		step func(t *testing.T, x *revFixture, ref config.ServerRef) []string
		gone bool
	}{
		{"the token gone", func(t *testing.T, x *revFixture, ref config.ServerRef) []string {
			t.Helper()
			from, _, err := x.deleteLogin(t.Context(), ref)
			require.NoError(t, err)
			// A loose mode set now is noticed only by the read after the
			// failed delete.
			require.NoError(t, os.Chmod(config.CredentialsFilePath(x.dir), 0o644))
			return from
		}, true},
		{"the token still stored", func(*testing.T, *revFixture, config.ServerRef) []string { return nil }, false},
		{"the store unreadable", func(t *testing.T, x *revFixture, _ config.ServerRef) []string {
			t.Helper()
			require.NoError(t, os.WriteFile(config.CredentialsFilePath(x.dir), []byte("{{ not yaml"), 0o600))
			return nil
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newRevFixture(t, testrancher.Options{})
			login := x.login("prod", "alice")
			next := x.login("dev", "alice")
			var calls []string
			failure := errors.New("removing the credential identity record: lock wait timed out")
			req := x.req(DeleteForLogout, "prod", "dev")
			req.DeleteLogin = func(ctx context.Context, ref config.ServerRef) ([]string, []string, error) {
				calls = append(calls, ref.Name)
				if ref.Name != "prod" {
					return x.deleteLogin(ctx, ref)
				}
				return tc.step(t, x, ref), nil, failure
			}
			d, rep, err := x.run(req)
			require.ErrorIs(t, err, failure)
			require.NotNil(t, d.LoginErr)
			assert.Equal(t, "prod", d.LoginErr.Target)
			assert.Equal(t, []string{"prod"}, calls, "the login deletes stop at the failure")
			assert.Empty(t, d.DeletedTargets())
			cfg := x.load()
			assert.Equal(t, next.Ref, cfg.Servers["dev"].TokenRef, "the next target's TokenRef is put back")
			assert.True(t, x.alive(next))
			assert.Equal(t, "notDeleted", notRevoked(t, rep, next.Ref.Name).Reason)
			e := cfg.Servers["prod"]
			assert.Equal(t, tc.gone, d.LoginDeleted("prod"))
			if !tc.gone {
				assert.Equal(t, "notDeleted", notRevoked(t, rep, login.Ref.Name).Reason)
				assert.Equal(t, login.Ref, e.TokenRef, "the TokenRef is put back")
				assert.True(t, x.alive(login))
				assert.Empty(t, d.DeletedFrom["prod"])
				return
			}
			assert.Equal(t, []string{"file"}, d.DeletedFrom["prod"])
			assert.Contains(t, revokedNames(rep), login.Ref.Name)
			assert.False(t, x.alive(login))
			assert.Nil(t, e.TokenRef, "the entry stays logged out")
			if goruntime.GOOS != "windows" {
				assert.Contains(t, strings.Join(d.Notices, "\n"), "is group/world-readable", "the read's own notice is kept")
			}
		})
	}
}

// TestATargetDeletedBeforeAFailedDeleteIsListed asserts that a target
// whose login delete completed before another target's delete failed
// after its token went stays among the deleted targets, while the failed
// one does not.
func TestATargetDeletedBeforeAFailedDeleteIsListed(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("dev", "alice")
	x.login("prod", "alice")
	req := x.req(DeleteForLogout, "dev", "prod")
	failure := errors.New("removing the credential identity record: lock wait timed out")
	req.DeleteLogin = func(ctx context.Context, ref config.ServerRef) ([]string, []string, error) {
		from, _, err := x.deleteLogin(ctx, ref)
		require.NoError(t, err)
		if ref.Name == "prod" {
			return from, nil, failure
		}
		return from, nil, nil
	}
	d, _, err := x.run(req)
	require.ErrorIs(t, err, failure)
	assert.True(t, d.LoginDeleted("prod"))
	assert.Equal(t, []string{"dev"}, d.DeletedTargets())
}
