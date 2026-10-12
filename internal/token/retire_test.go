package token

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gofrs/flock"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var retireUsers = []testrancher.User{
	{Name: "a", Password: "pw", UserID: "user-a"},
	{Name: "b", Password: "pw", UserID: "user-b"},
	{Name: "c", Password: "pw", UserID: "user-c"},
}

// retireFixture is a facade with a config dir holding one exec-login
// entry, "e", whose records the test seeds. On the loopback network srv
// is set and url is its URL; in memory (inside a synctest bubble) srv is
// nil, url is rancherOrigin and rt dispatches to the facade.
type retireFixture struct {
	f   *testrancher.Facade
	srv *httptest.Server
	url string
	rt  hostTransport
	dir string
}

func newRetireFixture(t *testing.T, opts testrancher.Options) *retireFixture {
	t.Helper()
	opts.Users = retireUsers
	f, srv := newTokenFacade(t, opts)
	x := &retireFixture{f: f, srv: srv, url: srv.URL}
	x.init(t)
	return x
}

// newInMemoryRetireFixture is newRetireFixture for a synctest bubble.
func newInMemoryRetireFixture(t *testing.T, opts testrancher.Options) *retireFixture {
	t.Helper()
	opts.Users = retireUsers
	f, rt := newInMemoryTokenFacade(t, opts)
	x := &retireFixture{f: f, url: rancherOrigin, rt: rt}
	x.init(t)
	return x
}

func (x *retireFixture) init(t *testing.T) {
	t.Helper()
	x.dir = t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{"e": {URL: x.url, Provenance: clierr.ProvenanceExecLogin}}}
	cfg.SetPath(config.FilePath(x.dir))
	require.NoError(t, cfg.Write())
}

// client reaches the fixture's facade; the credentials credAs acquires
// carry it, so the retirement pass's deletes go the same way.
func (x *retireFixture) client() *http.Client {
	if x.rt != nil {
		return x.rt.client()
	}
	return x.srv.Client()
}

// credAs acquires a derived credential for the named user.
func (x *retireFixture) credAs(t *testing.T, user string) LoginCredential {
	t.Helper()
	cred, err := AcquireLoginCredential(t.Context(), x.client(), LoginRequest{Server: x.url, AuthProvider: "localProvider", ResponseType: "json"}, 0,
		ui.New(&bytes.Buffer{}, newLineReader(user, "pw"), false, true))
	require.NoError(t, err)
	return cred
}

// record builds a superseded record for a live credential's token.
func record(c LoginCredential, at string) config.SupersededToken {
	return config.SupersededToken{Ref: *c.Ref, UserID: c.UserID, ExpiresAt: c.ExpiresAt, SupersededAt: at}
}

func (x *retireFixture) seedRecords(t *testing.T, recs []config.SupersededToken) {
	t.Helper()
	require.NoError(t, config.UpdateServerEntry(t.Context(), x.dir, "e", func(s *config.Server) bool {
		s.SupersededTokens = recs
		return true
	}))
}

func (x *retireFixture) records(t *testing.T) []config.SupersededToken {
	t.Helper()
	cfg, err := config.Load(config.FilePath(x.dir))
	require.NoError(t, err)
	return cfg.Servers["e"].SupersededTokens
}

// TestRetireSameOwnerRevokesWithTheNewCredentialAndDropsTheRecord asserts
// the core rule, and that a token already gone (404) is retired too.
func TestRetireSameOwnerRevokesWithTheNewCredentialAndDropsTheRecord(t *testing.T) {
	x := newRetireFixture(t, testrancher.Options{})
	old, fresh := x.credAs(t, "a"), x.credAs(t, "a")
	gone := config.SupersededToken{Ref: TokenRef{Kind: TokenKindExt, Name: "token-gone"}, UserID: "user-a", SupersededAt: "2026-09-01T00:00:00Z"}
	x.seedRecords(t, []config.SupersededToken{gone, record(old, "2026-09-02T00:00:00Z")})
	before := len(x.f.DeleteRequests())

	rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, fresh, x.records(t))
	assert.ElementsMatch(t, []TokenRef{gone.Ref, *old.Ref}, rep.Retired)
	assert.Empty(t, rep.Failed)
	assert.Empty(t, rep.Kept)
	assert.Empty(t, rep.Warnings)
	deletes := x.f.DeleteRequests()[before:]
	require.Len(t, deletes, 2)
	assert.Equal(t, "token-gone", deletes[0].Name, "oldest record first")
	assert.Equal(t, fresh.Token, deletes[1].Bearer, "the new credential is the bearer")
	assert.Empty(t, x.records(t))
	_, alive := x.f.TokenOwner(old.Ref.Name)
	assert.False(t, alive)
}

// TestRetireKeepsRecordsOfOtherOrUnknownOwnersWithoutAttempting asserts no
// delete is attempted for a different or empty owner, the records stay,
// and the warnings name the owner and carry the recovery sequence.
func TestRetireKeepsRecordsOfOtherOrUnknownOwnersWithoutAttempting(t *testing.T) {
	x := newRetireFixture(t, testrancher.Options{})
	bs, fresh := x.credAs(t, "b"), x.credAs(t, "a")
	unknown := record(x.credAs(t, "c"), "2026-09-03T00:00:00Z")
	unknown.UserID = ""
	x.seedRecords(t, []config.SupersededToken{record(bs, "2026-09-02T00:00:00Z"), unknown})
	before := len(x.f.DeleteRequests())

	rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, fresh, x.records(t))
	assert.Empty(t, rep.Retired)
	assert.Len(t, rep.Kept, 2)
	assert.Len(t, x.f.DeleteRequests(), before, "no delete is attempted on a guess")
	assert.Len(t, x.records(t), 2)
	require.Len(t, rep.Warnings, 2)
	assert.Contains(t, rep.Warnings[0], "user-b")
	assert.Contains(t, rep.Warnings[0], "different user")
	assert.Contains(t, rep.Warnings[0], "rancher auth login "+clierr.ShellQuote(clierr.RecoveryEntryName(x.url)))
	assert.Contains(t, rep.Warnings[0], clierr.RecoverySequenceAssumption)
	assert.Contains(t, rep.Warnings[1], "owner is not known")
}

// TestRetireFailedDeleteKeepsTheRecordStamped asserts a 5xx lands as
// failed with lastAttempt, the record kept, and a warning; the 403 case
// (a v3 record, a non-admin owner) is TestRetireV3ForbiddenKeepsTheRecord.
func TestRetireFailedDeleteKeepsTheRecordStamped(t *testing.T) {
	x := newRetireFixture(t, testrancher.Options{})
	old, fresh := x.credAs(t, "a"), x.credAs(t, "a")
	x.f.SetDeleteMode(old.Ref.Name, "fail")
	x.seedRecords(t, []config.SupersededToken{record(old, "2026-09-02T00:00:00Z")})
	rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, fresh, x.records(t))
	assert.Equal(t, []TokenRef{*old.Ref}, rep.Failed)
	recs := x.records(t)
	require.Len(t, recs, 1)
	require.NotNil(t, recs[0].LastAttempt)
	assert.Equal(t, clierr.ResultFailed, recs[0].LastAttempt.Result)
	assert.Contains(t, recs[0].LastAttempt.Error, "500")
	assert.Nil(t, recs[0].LastAttempt.Ref)
	require.Len(t, rep.Warnings, 1)
	assert.Contains(t, rep.Warnings[0], "could not be revoked")
}

// TestRetireKeepsAV3RecordUnattempted asserts that a v3 record is never
// deleted: no request is sent, the record is kept unstamped, and the
// warning names the administrator's delete and the expiry as the ways it
// ends.
func TestRetireKeepsAV3RecordUnattempted(t *testing.T) {
	x := newRetireFixture(t, testrancher.Options{Legacy: true, LegacyLoginBodyUserID: true})
	old, fresh := x.credAs(t, "a"), x.credAs(t, "a") // legacy: both are v3 session tokens with owner user-a
	require.Equal(t, TokenKindV3, old.Ref.Kind)
	x.seedRecords(t, []config.SupersededToken{record(old, "2026-09-02T00:00:00Z")})
	before := len(x.f.DeleteRequests())
	rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, fresh, x.records(t))
	assert.Empty(t, rep.Failed)
	assert.Empty(t, rep.Retired)
	assert.Len(t, rep.Kept, 1)
	assert.Len(t, x.f.DeleteRequests(), before, "no delete is sent for a v3 token")
	recs := x.records(t)
	require.Len(t, recs, 1)
	assert.Nil(t, recs[0].LastAttempt)
	require.Len(t, rep.Warnings, 1)
	assert.Contains(t, rep.Warnings[0], "the CLI cannot delete this v3 token")
	assert.Contains(t, rep.Warnings[0], "an administrator can delete it with: "+clierr.MarkRemedy("rancher delete management-token "+old.Ref.Name+" --apply --yes --allow-deprecated"))
	assert.Contains(t, rep.Warnings[0], "or it expires")
	_, alive := x.f.TokenOwner(old.Ref.Name)
	assert.True(t, alive)
}

// TestRetireKeptV3RecordNamesTheAdministratorsDelete asserts the warning
// for a v3 record the retirement pass keeps because of the login or the
// server, rather than because it is v3: the administrator's delete and the
// expiry, never the owner's login sequence or its assumption line.
func TestRetireKeptV3RecordNamesTheAdministratorsDelete(t *testing.T) {
	ref := TokenRef{Kind: TokenKindV3, Name: "kubeconfig-u-1"}
	rec := config.SupersededToken{Ref: ref, UserID: "user-a", ExpiresAt: "2999-01-01T00:00:00Z", SupersededAt: "2026-09-02T00:00:00Z"}
	for _, tc := range []struct {
		name, why string
		rec       config.SupersededToken
		pasted    bool
	}{
		{"a pasted login", "it waits for a login the CLI creates, and this login's token was pasted", rec, true},
		{"a record of another server", "it is a token of https://old.ex, which this entry no longer names",
			config.SupersededToken{Ref: ref, UserID: "user-a", ExpiresAt: rec.ExpiresAt, SupersededAt: rec.SupersededAt, Server: "https://old.ex"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newRetireFixture(t, testrancher.Options{})
			x.seedRecords(t, []config.SupersededToken{tc.rec})
			cred := UserSupplied("pasted")
			if !tc.pasted {
				cred = x.credAs(t, "a")
			}
			before := len(x.f.DeleteRequests())
			rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, cred, x.records(t))
			assert.Len(t, rep.Kept, 1)
			require.Len(t, rep.Warnings, 1)
			assert.Equal(t, "the superseded token v3/kubeconfig-u-1 was not retired: "+tc.why+"; "+v3AdminRemedy(ref, rec.ExpiresAt), rep.Warnings[0])
			assert.NotContains(t, rep.Warnings[0], clierr.RecoverySequenceAssumption)
			assert.NotContains(t, rep.Warnings[0], "auth login")
			assert.Len(t, x.f.DeleteRequests(), before)
		})
	}
}

// TestRetireOwnersABCThenA asserts A's record is retired and B's and C's
// are kept when A logs in again.
func TestRetireOwnersABCThenA(t *testing.T) {
	x := newRetireFixture(t, testrancher.Options{})
	a1, b, c, a2 := x.credAs(t, "a"), x.credAs(t, "b"), x.credAs(t, "c"), x.credAs(t, "a")
	x.seedRecords(t, []config.SupersededToken{record(a1, "2026-09-01T00:00:00Z"), record(b, "2026-09-02T00:00:00Z"), record(c, "2026-09-03T00:00:00Z")})
	rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, a2, x.records(t))
	assert.Equal(t, []TokenRef{*a1.Ref}, rep.Retired)
	recs := x.records(t)
	require.Len(t, recs, 2)
	assert.Equal(t, "user-b", recs[0].UserID)
	assert.Equal(t, "user-c", recs[1].UserID)
}

// TestRetireBudgetBoundsHangingDeletes asserts one total budget: with
// every same-owner delete hanging, the pass returns within the budget plus
// the lock wait, the first record carries a failed attempt, and the
// unreached records stay unattempted and unstamped.
func TestRetireBudgetBoundsHangingDeletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		x := newInMemoryRetireFixture(t, testrancher.Options{})
		r1, r2, r3, fresh := x.credAs(t, "a"), x.credAs(t, "a"), x.credAs(t, "a"), x.credAs(t, "a")
		for _, c := range []LoginCredential{r1, r2, r3} {
			x.f.SetDeleteMode(c.Ref.Name, "hang")
		}
		x.seedRecords(t, []config.SupersededToken{record(r1, "2026-09-01T00:00:00Z"), record(r2, "2026-09-02T00:00:00Z"), record(r3, "2026-09-03T00:00:00Z")})
		start := time.Now()
		rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, fresh, x.records(t)) // t.Context carries no deadline: the deadline-less caller
		assert.Equal(t, retirementBudget, time.Since(start), "the first hanging delete spends the whole budget on the fake clock; nothing else waits")
		assert.Len(t, rep.Failed, 1)
		assert.Len(t, rep.Kept, 2, "unreached records are kept")
		recs := x.records(t)
		require.Len(t, recs, 3)
		assert.NotNil(t, recs[0].LastAttempt)
		assert.Nil(t, recs[1].LastAttempt, "unreached records are unstamped")
		assert.Nil(t, recs[2].LastAttempt)
		assert.Contains(t, rep.Warnings[len(rep.Warnings)-1], "not attempted")
	})
}

// TestRetireRecordWriteLockTimeoutLeavesRecordsAndSucceeds asserts a
// contended config lock at the record write is a warning: the deletes
// happened, the records stay, the pass returns normally. It runs on real
// time with a shortened lock wait: the contention is on the process-wide
// config lock semaphore, a channel created outside any bubble, so a wait
// on it is not durably blocked and a bubble's clock would never advance.
func TestRetireRecordWriteLockTimeoutLeavesRecordsAndSucceeds(t *testing.T) {
	restore := config.SetLockWaitTimeoutForTest(200 * time.Millisecond)
	t.Cleanup(restore)
	x := newRetireFixture(t, testrancher.Options{})
	old, fresh := x.credAs(t, "a"), x.credAs(t, "a")
	x.seedRecords(t, []config.SupersededToken{record(old, "2026-09-02T00:00:00Z")})
	held, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = config.WithLock(t.Context(), x.dir, func() error { close(held); <-release; return nil })
	})
	<-held
	rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, fresh, x.records(t))
	close(release)
	wg.Wait()
	assert.Equal(t, []TokenRef{*old.Ref}, rep.Retired)
	assert.Len(t, x.records(t), 1, "the record stays: redundant, never lost")
	require.NotEmpty(t, rep.Warnings)
	assert.Contains(t, rep.Warnings[len(rep.Warnings)-1], "could not update")
}

// TestRetireNoRequestUnderTheConfigLockAndReloadsBeforeWriting asserts no
// delete runs while the config lock is held, and that the record write
// reloads the config under the lock (a concurrent entry written just
// before the reload survives).
func TestRetireNoRequestUnderTheConfigLockAndReloadsBeforeWriting(t *testing.T) {
	var violations atomic.Int32
	var dir string
	x := newRetireFixture(t, testrancher.Options{RequestHook: func(*http.Request) {
		fl := flock.New(config.LockPath(dir))
		ok, err := fl.TryLock()
		if err != nil || !ok {
			violations.Add(1)
			return
		}
		_ = fl.Unlock()
	}})
	dir = x.dir
	old, fresh := x.credAs(t, "a"), x.credAs(t, "a")
	x.seedRecords(t, []config.SupersededToken{record(old, "2026-09-02T00:00:00Z")})
	restore := config.SetLockHookForTest(func() {
		cfg, _ := config.Load(config.FilePath(dir))
		cfg.Servers["sentinel"] = config.Server{URL: "https://sentinel.ex"}
		_ = cfg.Write()
	})
	t.Cleanup(restore)
	rep := RetireSuperseded(t.Context(), x.dir, "e", x.url, fresh, x.records(t))
	assert.Equal(t, int32(0), violations.Load())
	assert.Len(t, rep.Retired, 1)
	cfg, _ := config.Load(config.FilePath(x.dir))
	assert.Contains(t, cfg.Servers, "sentinel", "the record write reloads under the lock, never clobbers")
	assert.Empty(t, cfg.Servers["e"].SupersededTokens)
}

// TestV3RemedyTexts asserts the two v3 delete texts: the superseded-token
// and revocation warnings (the CLI cannot delete this v3 token: an
// administrator's delete, or its expiry) and the residual text of a v3
// token a login could not delete, which names the token and offers no hint.
func TestV3RemedyTexts(t *testing.T) {
	t.Parallel()
	ref := TokenRef{Kind: TokenKindV3, Name: "token-v3"}
	cmd := clierr.MarkRemedy("rancher delete management-token token-v3 --apply --yes --allow-deprecated")
	assert.Equal(t, "the CLI cannot delete this v3 token; an administrator can delete it with: "+cmd+", or it expires at 2026-10-06T00:00:00Z",
		v3Remedy(ref, "2026-10-06T00:00:00Z"))
	assert.Equal(t, "the CLI cannot delete this v3 token; an administrator can delete it with: "+cmd+", or it expires", v3Remedy(ref, ""))
	assert.Equal(t, "an administrator can delete v3/token-v3 with: "+cmd+", or it expires at 2026-10-06T00:00:00Z",
		v3AdminRemedy(ref, "2026-10-06T00:00:00Z"))
	assert.Equal(t, "an administrator can delete v3/token-v3 with: "+cmd+", or it expires", v3AdminRemedy(ref, ""))

	fate := clierr.TokenFate{Ref: &clierr.TokenRef{Kind: clierr.TokenKindV3, Name: "token-v3"}, ExpiresAt: "2026-10-06T00:00:00Z"}
	text, hint := ResidualRecovery(fate, "prod", "m", true)
	assert.Equal(t, v3AdminRemedy(ref, fate.ExpiresAt), text)
	assert.Empty(t, hint, "the administrator's delete is not valid verbatim for every caller")
	text, hint = ResidualRecovery(fate, "prod", "m", false)
	login, _ := reLogin("prod")
	assert.Equal(t, "no credential was stored; "+login+". A"+strings.TrimPrefix(v3AdminRemedy(ref, fate.ExpiresAt), "a"), text)
	assert.Empty(t, hint)
	text, hint = ResidualRecovery(clierr.TokenFate{Ref: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "t"}}, "prod", "m", true)
	assert.Contains(t, text, "revoke ext/t with: ", "an ext token keeps its revoke command")
	assert.NotEmpty(t, hint)
}
