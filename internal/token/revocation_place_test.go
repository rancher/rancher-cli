package token

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWhereACacheTokenIsRecorded asserts the placement of a cache record
// by its kind: a plugin answer for an identified entry is recorded on the
// plugin login entry, one with token reuse off on the identified entry;
// a server-wide record goes to the first non-empty candidate group of its
// command - a logout's target whatever the owners, the others first under
// delete-server - a known same owner preferred within the group; a listed
// entry that now names another server is not a candidate.
func TestWhereACacheTokenIsRecorded(t *testing.T) {
	t.Run("a plugin answer is recorded on the plugin login entry", func(t *testing.T) {
		x := newRevFixture(t, testrancher.Options{})
		x.edit(func(cfg *config.Config) { cfg.TokenReuse = true })
		x.login("prod", "alice")
		x.entry("exec-login-x", config.Server{Provenance: clierr.ProvenanceExecLogin, TokenUserID: "user-alice"})
		ct := x.token("alice")
		x.cache("k", ct, config.CacheBindingForIdentified(config.CanonicalDir(x.dir), x.load(), x.url, "prod", ""), true)
		_, _, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		assert.False(t, x.alive(ct), "the record names prod in its binding, so prod's logout deletes and revokes it")
	})
	t.Run("with token reuse off on the identified entry", func(t *testing.T) {
		x := newRevFixture(t, testrancher.Options{})
		x.login("prod", "alice")
		ct := x.token("alice")
		x.f.SetDeleteMode(ct.Ref.Name, "fail")
		x.cache("k", ct, config.CacheBindingForIdentified(config.CanonicalDir(x.dir), x.load(), x.url, "prod", ""), true)
		_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
		require.NoError(t, err)
		assert.Equal(t, "prod", notRevoked(t, rep, ct.Ref.Name).Entry)
	})
	for _, tc := range []struct {
		name      string
		cmd       DeleteCommand
		owner     string
		wantEntry string
	}{
		{"logout: the target, whatever the owners", DeleteForLogout, "user-bob", "prod"},
		{"delete-server: another listed entry first", DeleteForDeleteServer, "user-alice", "dev"},
		{"delete-server: the same owner preferred", DeleteForDeleteServer, "user-carol", "qa"},
		{"delete-server: an empty owner takes the first by name", DeleteForDeleteServer, "", "dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newRevFixture(t, testrancher.Options{})
			x.login("prod", "alice")
			x.entry("dev", config.Server{TokenUserID: "user-dan"})
			x.entry("qa", config.Server{TokenUserID: "user-carol"})
			x.entry("moved", config.Server{TokenUserID: tc.owner})
			ct := x.token("alice")
			x.f.SetDeleteMode(ct.Ref.Name, "fail")
			x.cache("k", ct, x.serverWide().WithCreatedToken(ct.Ref, tc.owner), false)
			x.edit(func(cfg *config.Config) {
				s := cfg.Servers["moved"]
				s.URL = "https://elsewhere.ex"
				cfg.Servers["moved"] = s
			})
			_, rep, err := x.run(x.req(tc.cmd, "prod"))
			require.NoError(t, err)
			n := notRevoked(t, rep, ct.Ref.Name)
			assert.Equal(t, tc.wantEntry, n.Entry)
			assert.Equal(t, "failed", n.Reason, "the reason is the revoke outcome, whoever the owner")
		})
	}
	t.Run("no candidate is noEntry", func(t *testing.T) {
		x := newRevFixture(t, testrancher.Options{})
		x.login("prod", "alice")
		ct := x.token("alice")
		x.cache("k", ct, x.serverWide(), true)
		x.edit(func(cfg *config.Config) {
			s := cfg.Servers["prod"]
			s.URL = "https://elsewhere.ex"
			cfg.Servers["prod"] = s
		})
		// prod names another server now; delete-token picks the record.
		req := x.req(DeleteForDeleteToken)
		req.Select = func(config.CachedRecord) bool { return true }
		_, rep, err := x.run(req)
		require.NoError(t, err)
		n := notRevoked(t, rep, ct.Ref.Name)
		assert.Equal(t, "noEntry", n.Reason)
		assert.Empty(t, n.Entry)
		assert.True(t, x.alive(ct))
	})
}

// TestABearerGoesOnlyToAServerAnEntryNames asserts that a record whose
// parent entry exists but whose server no current entry names is recorded
// there with its server and reported noEntry, manual, with nothing sent;
// and that when a second entry names the record's server the request goes
// there, while retry stays manual: the parent no longer names the server,
// so its next login cannot retire it.
func TestABearerGoesOnlyToAServerAnEntryNames(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	ct := x.token("alice")
	x.f.SetDeleteMode(ct.Ref.Name, "fail")
	x.cache("k", ct, x.own("prod"), true)
	x.edit(func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		s.URL = "https://elsewhere.ex"
		cfg.Servers["prod"] = s
	})
	req := x.req(DeleteForDeleteToken)
	req.Select = func(config.CachedRecord) bool { return true }
	before := len(x.f.DeleteRequests())
	_, rep, err := x.run(req)
	require.NoError(t, err)
	n := notRevoked(t, rep, ct.Ref.Name)
	assert.Equal(t, "noEntry", n.Reason)
	assert.Equal(t, "prod", n.Entry)
	assert.Equal(t, "manual", n.Retry)
	assert.Len(t, x.f.DeleteRequests(), before, "no bearer leaves")
	recs := x.load().Servers["prod"].SupersededTokens
	require.Len(t, recs, 1)
	assert.Equal(t, config.CanonicalServerIdentity(x.url), recs[0].Server)

	y := newRevFixture(t, testrancher.Options{})
	y.login("prod", "alice")
	ct2 := y.token("alice")
	y.f.SetDeleteMode(ct2.Ref.Name, "fail")
	y.cache("k", ct2, y.own("prod"), true)
	y.entry("second", config.Server{TokenUserID: "user-alice"})
	y.edit(func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		s.URL = "https://elsewhere.ex"
		cfg.Servers["prod"] = s
	})
	req = y.req(DeleteForDeleteToken)
	req.Select = func(config.CachedRecord) bool { return true }
	before = len(y.f.DeleteRequests())
	_, rep, err = y.run(req)
	require.NoError(t, err)
	n = notRevoked(t, rep, ct2.Ref.Name)
	assert.Equal(t, "failed", n.Reason, "sent through the entry that names the server")
	assert.Equal(t, "prod", n.Entry)
	assert.Equal(t, "manual", n.Retry, "prod no longer names the token's server")
	assert.Len(t, y.f.DeleteRequests()[before:], 1)
}

// TestTheTrustOfTheEntryThatNamesTheServer asserts the trust choice when
// the parent names another server and two entries name the record's: the
// one with a CA is used, so the request verifies against the server's
// private CA.
func TestTheTrustOfTheEntryThatNamesTheServer(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	tlsF := testrancher.New(testrancher.Options{Users: revocationUsers})
	tlsSrv := httptest.NewTLSServer(tlsF)
	t.Cleanup(tlsSrv.Close)
	ca := certPEM(t, tlsSrv)
	ct := tokenOn(t, tlsSrv, "alice")
	x.entry("prod", config.Server{URL: tlsSrv.URL})
	b := config.CacheBindingFor(config.CanonicalDir(x.dir), x.load(), "prod").WithCreatedToken(ct.Ref, "user-alice")
	require.NoError(t, x.store.SetCachedCred("k", ct.Token, time.Now().Add(time.Hour), "k", config.CanonicalServerIdentity(tlsSrv.URL), "c-1", b))
	x.edit(func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		s.URL = "https://elsewhere.ex"
		cfg.Servers["prod"] = s
		cfg.Servers["a-plain"] = config.Server{URL: tlsSrv.URL}
		cfg.Servers["b-with-ca"] = config.Server{URL: tlsSrv.URL, CA: ca}
	})
	req := x.req(DeleteForDeleteToken)
	req.Select = func(config.CachedRecord) bool { return true }
	_, rep, err := x.run(req)
	require.NoError(t, err)
	assert.Equal(t, []string{ct.Ref.Name}, revokedNames(rep))
}

// TestEachServerUsesItsOwnCA asserts a logout --all across two servers,
// each behind its own private CA: every token is revoked through its own
// entry's trust.
func TestEachServerUsesItsOwnCA(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	var creds []LoginCredential
	for _, name := range []string{"one", "two"} {
		f := testrancher.New(testrancher.Options{Users: revocationUsers})
		srv := httptest.NewTLSServer(f)
		t.Cleanup(srv.Close)
		cred := tokenOn(t, srv, "alice")
		creds = append(creds, cred)
		x.entry(name, config.Server{URL: srv.URL, CA: certPEM(t, srv), Provenance: clierr.ProvenanceDerived, TokenRef: cred.Ref, TokenUserID: "user-alice"})
		require.NoError(t, x.store.SetToken(config.ServerRef{Name: name, URL: srv.URL}, cred.Token))
	}
	_, rep, err := x.run(x.req(DeleteForLogoutAll, "one", "two"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{creds[0].Ref.Name, creds[1].Ref.Name}, revokedNames(rep))
	assert.Empty(t, rep.NotRevoked)
}

// TestAFailedRestoreAndTheRerun asserts the state a failed restore write
// leaves and its convergence: the login stays stored with its record and
// a nil TokenRef, reported notDeleted with its entry; the rerun reads that
// login and revokes the record with the token as its own bearer. The rule
// is not applied while the login delete keeps failing.
func TestAFailedRestoreAndTheRerun(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	login := x.login("prod", "alice")
	failing := x.req(DeleteForLogout, "prod")
	failing.DeleteLogin = func(context.Context, config.ServerRef) ([]string, []string, error) {
		require.NoError(t, os.Chmod(x.dir, 0o500))
		return nil, nil, errors.New("the helper refused")
	}
	d, err := x.localDelete(failing)
	require.NoError(t, os.Chmod(x.dir, 0o700))
	require.Error(t, err)
	d.RevokeTokens(t.Context())
	rep := d.Report()
	n := notRevoked(t, rep, login.Ref.Name)
	assert.Equal(t, "notDeleted", n.Reason)
	assert.Equal(t, "prod", n.Entry, "the failed restore leaves the record")
	assert.Equal(t, "manual", n.Retry)
	s := x.load().Servers["prod"]
	assert.Nil(t, s.TokenRef)
	require.Len(t, s.SupersededTokens, 1)

	again := x.req(DeleteForLogout, "prod")
	again.DeleteLogin = func(context.Context, config.ServerRef) ([]string, []string, error) {
		return nil, nil, errors.New("the helper refused again")
	}
	before := len(x.f.DeleteRequests())
	_, rep, err = x.run(again)
	require.Error(t, err)
	assert.Equal(t, "noBearer", notRevoked(t, rep, login.Ref.Name).Reason, "the login was not deleted: no own bearer")
	assert.Len(t, x.f.DeleteRequests(), before)

	before = len(x.f.DeleteRequests())
	_, rep, err = x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	assert.Equal(t, []string{login.Ref.Name}, revokedNames(rep))
	assert.Empty(t, rep.NotRevoked, "the stamped entry holds no login of its own to report")
	deletes := x.f.DeleteRequests()[before:]
	require.Len(t, deletes, 1)
	assert.Equal(t, login.Token, deletes[0].Bearer)
	assert.Empty(t, x.load().Servers["prod"].SupersededTokens)
}

// TestLogoutAllWithAFailedLoginDelete asserts --all after one login
// delete fails: that target and every later one are restored and their
// logins notDeleted, the second hold still deletes every planned cache
// record, and those tokens are revoked.
func TestLogoutAllWithAFailedLoginDelete(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	a := x.login("a", "alice")
	b := x.login("b", "alice")
	c := x.login("c", "alice")
	ct := x.token("alice")
	x.cache("ct", ct, x.own("c"), true)
	req := x.req(DeleteForLogoutAll, "a", "b", "c")
	req.DeleteLogin = func(ctx context.Context, ref config.ServerRef) ([]string, []string, error) {
		if ref.Name == "b" {
			return nil, nil, errors.New("the keychain refused")
		}
		return x.deleteLogin(ctx, ref)
	}
	_, rep, err := x.run(req)
	require.Error(t, err)
	assert.ElementsMatch(t, []string{a.Ref.Name, ct.Ref.Name}, revokedNames(rep))
	for _, cred := range []LoginCredential{b, c} {
		assert.Equal(t, "notDeleted", notRevoked(t, rep, cred.Ref.Name).Reason)
		assert.True(t, x.alive(cred))
	}
	cfg := x.load()
	assert.Nil(t, cfg.Servers["a"].TokenRef)
	assert.Equal(t, b.Ref, cfg.Servers["b"].TokenRef)
	assert.Equal(t, c.Ref, cfg.Servers["c"].TokenRef)
	assert.Empty(t, cfg.Servers["c"].LoggedOutAt)
	assert.Empty(t, x.cacheKeys())
}

// TestARerunReportsTheLeftoversOnce asserts the rerun after a run whose
// revoke failed: the record left is classified by the superseded row
// (noBearer: no login is stored), reported once, and only the binding id
// and the stamp change.
func TestARerunReportsTheLeftoversOnce(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	ct := x.token("alice")
	x.cache("ct", ct, x.own("prod"), true)
	x.f.SetDeleteMode(ct.Ref.Name, "fail")
	_, _, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	first := x.load().Servers["prod"]

	before := len(x.f.DeleteRequests())
	_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	require.Len(t, rep.NotRevoked, 1)
	assert.Equal(t, "noBearer", rep.NotRevoked[0].Reason)
	assert.Equal(t, "nextLogin", rep.NotRevoked[0].Retry)
	assert.Len(t, x.f.DeleteRequests(), before)
	second := x.load().Servers["prod"]
	assert.NotEqual(t, first.CacheBindingID, second.CacheBindingID)
	assert.Equal(t, first.SupersededTokens, second.SupersededTokens)
}

// TestACrashAfterTheLocalDeleteConvergesAtTheNextLogin asserts that records
// left by a run killed after the local delete are retired by the next
// login the CLI creates for the same user, and left unstamped by a pasted
// one.
func TestACrashAfterTheLocalDeleteConvergesAtTheNextLogin(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	login := x.login("prod", "alice")
	ct := x.token("alice")
	x.cache("ct", ct, x.own("prod"), true)
	_, err := x.localDelete(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	recs := x.load().Servers["prod"].SupersededTokens
	require.Len(t, recs, 2)

	rep := RetireSuperseded(t.Context(), x.dir, "prod", x.url, UserSupplied("pasted:secret"), recs)
	assert.Empty(t, rep.Retired)
	for _, r := range x.load().Servers["prod"].SupersededTokens {
		assert.Nil(t, r.LastAttempt)
	}

	rep = RetireSuperseded(t.Context(), x.dir, "prod", x.url, x.token("alice"), recs)
	assert.ElementsMatch(t, []TokenRef{*login.Ref, *ct.Ref}, rep.Retired)
	assert.Empty(t, x.load().Servers["prod"].SupersededTokens)
}

// TestARecordWithAnOlderBindingIDIsInTheNextLogoutsScope asserts that a
// record a racing writer left bound to the entry's previous id is deleted
// and revoked by the entry's next logout.
func TestARecordWithAnOlderBindingIDIsInTheNextLogoutsScope(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	ct := x.token("alice")
	x.cache("ct", ct, x.own("prod"), true)
	x.edit(func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		require.NoError(t, config.AdvanceCacheBinding(&s))
		cfg.Servers["prod"] = s
	})
	_, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	assert.Contains(t, revokedNames(rep), ct.Ref.Name)
	assert.Empty(t, x.cacheKeys())
}

// TestAReaderAfterTheAdvanceFindsTheRecordsPlanned asserts that a reader
// evicting a record right after the first write cannot strand its token:
// the record was planned and recorded under the first hold, and its token
// is revoked though the second hold finds it gone.
func TestAReaderAfterTheAdvanceFindsTheRecordsPlanned(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	ct := x.token("alice")
	x.cache("ct", ct, x.own("prod"), true)
	req := x.req(DeleteForLogout, "prod")
	req.DeleteLogin = func(ctx context.Context, ref config.ServerRef) ([]string, []string, error) {
		anchor := config.NewCacheAnchor(x.dir).WithConfig(x.load())
		_, _, gerr := x.store.GetCachedCred("ct", anchor)
		require.ErrorIs(t, gerr, config.ErrNoToken, "the advanced binding makes the record a miss")
		assert.Empty(t, x.cacheKeys(), "the reader evicted it")
		return x.deleteLogin(ctx, ref)
	}
	_, rep, err := x.run(req)
	require.NoError(t, err)
	assert.Contains(t, revokedNames(rep), ct.Ref.Name)
}

// TestDeleteTokenKeepsARecordRewrittenBetweenTheHolds asserts auth
// delete-token's compare: the record the plugin rewrote is kept and named
// in Kept; and that a failed second hold removes the records the first
// write added and skips their tokens, which the cache still serves.
func TestDeleteTokenKeepsARecordRewrittenBetweenTheHolds(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	ct := x.token("alice")
	x.cache("ct", ct, x.own("prod"), true)
	fresh := x.token("alice")
	req := x.req(DeleteForDeleteToken)
	req.Select = func(config.CachedRecord) bool { return true }
	req.Cache = rewriteBetweenHolds{CredStore: x.store, rewrite: func() {
		x.cache("ct", fresh, x.own("prod"), true)
	}}
	d, rep, err := x.run(req)
	require.NoError(t, err)
	assert.Equal(t, []string{"ct"}, d.Kept)
	assert.Empty(t, d.DeletedCache)
	assert.Contains(t, revokedNames(rep), ct.Ref.Name, "the token read is no longer cached")
	assert.True(t, x.alive(fresh))

	y := newRevFixture(t, testrancher.Options{})
	y.login("prod", "alice")
	ct2 := y.token("alice")
	y.cache("ct", ct2, y.own("prod"), true)
	req = y.req(DeleteForDeleteToken)
	req.Select = func(config.CachedRecord) bool { return true }
	req.Cache = failingSecondHold{CredStore: y.store}
	before := len(y.f.DeleteRequests())
	_, rep, err = y.run(req)
	require.Error(t, err)
	n := notRevoked(t, rep, ct2.Ref.Name)
	assert.Equal(t, "notDeleted", n.Reason)
	assert.Len(t, y.f.DeleteRequests(), before)
	assert.Empty(t, y.load().Servers["prod"].SupersededTokens, "the settle span removes what the first write added")
	assert.Equal(t, []string{"ct"}, y.cacheKeys())
}

type rewriteBetweenHolds struct {
	config.CredStore
	rewrite func()
}

func (s rewriteBetweenHolds) DeleteCachedCredsIfUnchanged(want map[string]string) ([]string, []string, error) {
	s.rewrite()
	return s.CredStore.DeleteCachedCredsIfUnchanged(want)
}

type failingSecondHold struct{ config.CredStore }

func (failingSecondHold) DeleteCachedCredsIfUnchanged(map[string]string) ([]string, []string, error) {
	return nil, nil, errors.New("locking token-cache.yaml timed out")
}

// TestDeleteTokenWithoutConfig asserts that with no config.yaml every
// record is noEntry and nothing is sent or written.
func TestDeleteTokenWithoutConfig(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	ct := x.token("alice")
	x.cache("ct", ct, x.own("prod"), true)
	require.NoError(t, os.Remove(config.FilePath(x.dir)))
	req := x.req(DeleteForDeleteToken)
	req.Select = func(config.CachedRecord) bool { return true }
	before := len(x.f.DeleteRequests())
	_, rep, err := x.run(req)
	require.NoError(t, err)
	assert.Equal(t, "noEntry", notRevoked(t, rep, ct.Ref.Name).Reason)
	assert.Len(t, x.f.DeleteRequests(), before)
	_, serr := os.Stat(config.FilePath(x.dir))
	assert.True(t, os.IsNotExist(serr), "no config is written")
}

// TestDeleteTokenAllWithAnUnreadableCache asserts that --all removes a
// cache file it cannot read and says so, revoking and recording nothing.
func TestDeleteTokenAllWithAnUnreadableCache(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	require.NoError(t, os.WriteFile(x.dir+"/token-cache.yaml", []byte("{not yaml"), 0o600))
	d, rep, err := x.run(x.req(DeleteForDeleteToken))
	require.NoError(t, err)
	assert.True(t, d.CacheUnreadable)
	assert.Empty(t, rep.Revoked)
	assert.Empty(t, rep.NotRevoked)
	_, serr := os.Stat(x.dir + "/token-cache.yaml")
	assert.True(t, os.IsNotExist(serr))
}

// TestUpdateServerRecordsTheOldServersTokens asserts update-server's
// placement and outcome: nothing is sent; the moved entry's login and
// records are recorded on it with the old server, otherServer and manual;
// a token whose parent is another entry still naming the old server is
// recorded there, notAttempted, with nextLogin when that entry records
// its owner.
func TestUpdateServerRecordsTheOldServersTokens(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	login := x.login("prod", "alice")
	old := x.token("alice")
	x.edit(func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		s.SupersededTokens = []config.SupersededToken{
			{Ref: *old.Ref, UserID: "user-alice", SupersededAt: "2026-10-01T00:00:00Z"},
			{Ref: TokenRef{Kind: TokenKindExt, Name: "token-older"}, UserID: "user-alice", SupersededAt: "2026-09-01T00:00:00Z", Server: "https://older.example.com"},
		}
		cfg.Servers["prod"] = s
	})
	x.entry("dev", config.Server{TokenUserID: "user-alice"})
	mine, devs := x.token("alice"), x.token("alice")
	x.cache("mine", mine, x.own("prod"), true)
	x.cache("server", devs, x.serverWide(), true)
	before := len(x.f.DeleteRequests())

	d, err := x.localDelete(x.req(DeleteForUpdateServer, "prod"))
	require.NoError(t, err)
	d.RevokeTokens(t.Context())
	assert.Empty(t, d.Settle(t.Context()))
	rep := d.Report()
	assert.Len(t, x.f.DeleteRequests(), before, "update-server sends nothing")
	assert.Empty(t, rep.Revoked)
	server := config.CanonicalServerIdentity(x.url)
	for _, name := range []string{login.Ref.Name, old.Ref.Name, mine.Ref.Name} {
		n := notRevoked(t, rep, name)
		assert.Equal(t, "otherServer", n.Reason, name)
		assert.Equal(t, "prod", n.Entry, name)
		assert.Equal(t, "manual", n.Retry, name)
	}
	n := notRevoked(t, rep, devs.Ref.Name)
	assert.Equal(t, "notAttempted", n.Reason)
	assert.Equal(t, "dev", n.Entry)
	assert.Equal(t, "nextLogin", n.Retry)
	cfg := x.load()
	for _, r := range cfg.Servers["prod"].SupersededTokens {
		want := server
		if r.Ref.Name == "token-older" {
			want = "https://older.example.com"
		}
		assert.Equal(t, want, r.Server, "a record that already names a server keeps it: %s", r.Ref.Name)
	}
	assert.Len(t, cfg.Servers["prod"].SupersededTokens, 4)
	require.Len(t, cfg.Servers["dev"].SupersededTokens, 1)
	assert.Empty(t, x.cacheKeys())
}

// TestRemovedEntryRecordsAreManual asserts delete-server's report: tokens
// recorded on its entry keep that entry and the owner rule's retry until
// the entry is gone (or left with no login for the rerun to remove), and
// then carry no entry and are manual with their recovery.
func TestRemovedEntryRecordsAreManual(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	ct := x.token("alice")
	x.f.SetDeleteMode(ct.Ref.Name, "fail")
	x.cache("ct", ct, x.own("prod"), true)
	d, rep, err := x.run(x.req(DeleteForDeleteServer, "prod"))
	require.NoError(t, err)
	n := notRevoked(t, rep, ct.Ref.Name)
	assert.Equal(t, "prod", n.Entry)
	assert.Equal(t, "nextLogin", n.Retry)
	d.RemoveEntry("prod")
	n = notRevoked(t, d.Report(), ct.Ref.Name)
	assert.Empty(t, n.Entry)
	assert.Equal(t, "manual", n.Retry)
	assert.NotEmpty(t, n.Recovery)
}

// TestWarningsAndText asserts the warnings - one per manual token with its
// remedy marked, one summary per entry for its nextLogin tokens - and the
// text lines under each server.
func TestWarningsAndText(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	ct, bobs := x.token("alice"), x.token("bob")
	x.f.SetDeleteMode(ct.Ref.Name, "fail")
	x.cache("ct", ct, x.own("prod"), true)
	x.edit(func(cfg *config.Config) {
		s := cfg.Servers["prod"]
		s.SupersededTokens = []config.SupersededToken{{Ref: *bobs.Ref, UserID: "user-bob", SupersededAt: "2026-10-01T00:00:00Z"}}
		cfg.Servers["prod"] = s
	})
	d, _, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	w := d.Warnings()
	require.Len(t, w, 2)
	assert.Contains(t, w[0], "the superseded token "+bobs.Ref.String()+" was not revoked (otherOwner); revoke it with: "+clierr.MarkRemedy("rancher auth login"))
	assert.Equal(t, "1 token(s) recorded on prod are revoked by the next login of prod as user-alice ("+clierr.MarkRemedy("rancher auth login 'prod'")+")", w[1])
	lines := d.TextLines()
	assert.Equal(t, "tokens on "+config.CanonicalServerIdentity(x.url)+":", lines[0])
	assert.Contains(t, lines, "  not revoked "+ct.Ref.String()+" (failed: 500 Internal Server Error; recorded on prod: the next login of prod as user-alice revokes it)")
	assert.Contains(t, lines, "  not revoked "+bobs.Ref.String()+" (otherOwner; recorded on prod)")
	assert.Contains(t, lines[len(lines)-1], "revoked ", "the login, last")
}

// TestRefreshJudgesTheReportByTheConfigGiven asserts that the config a
// later span loaded, once given, is what the report's retry and next-login
// text are judged by: an entry a plugin login now holds names a kubectl
// sign-in, and an entry moved to another URL turns the token manual.
func TestRefreshJudgesTheReportByTheConfigGiven(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	ct := x.token("alice")
	x.f.SetDeleteMode(ct.Ref.Name, "fail")
	x.cache("ct", ct, x.own("prod"), true)
	d, rep, err := x.run(x.req(DeleteForLogout, "prod"))
	require.NoError(t, err)
	require.Equal(t, "nextLogin", notRevoked(t, rep, ct.Ref.Name).Retry)

	plugin := x.load()
	s := plugin.Servers["prod"]
	s.Provenance = clierr.ProvenanceExecLogin
	plugin.Servers["prod"] = s
	d.Refresh(plugin)
	assert.Contains(t, d.Warnings(), "1 token(s) recorded on prod are revoked by the next kubectl sign-in through prod as user-alice")

	moved := x.load()
	s = moved.Servers["prod"]
	s.URL = "https://moved.ex"
	moved.Servers["prod"] = s
	d.Refresh(moved)
	n := notRevoked(t, d.Report(), ct.Ref.Name)
	assert.Equal(t, "manual", n.Retry)
	assert.NotEmpty(t, n.Recovery)
}
