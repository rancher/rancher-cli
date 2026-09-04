package token

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
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

// retireFixture holds one facade and a config dir with the entry "e" whose
// records the test seeds.
type retireFixture struct {
	f   *testrancher.Facade
	srv *httptest.Server
	dir string
}

func newRetireFixture(t *testing.T, opts testrancher.Options) *retireFixture {
	t.Helper()
	opts.Users = retireUsers
	f, srv := newTokenFacade(t, opts)
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{"e": {URL: srv.URL, Provenance: clierr.ProvenanceExecLogin}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return &retireFixture{f: f, srv: srv, dir: dir}
}

// credAs acquires a derived credential for the named user.
func (x *retireFixture) credAs(t *testing.T, user string) LoginCredential {
	t.Helper()
	cred, err := AcquireLoginCredential(t.Context(), x.srv.Client(), LoginRequest{Server: x.srv.URL, AuthProvider: "localProvider", ResponseType: "json"}, 0,
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

	rep := RetireSuperseded(t.Context(), x.dir, "e", x.srv.URL, fresh, x.records(t))
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

	rep := RetireSuperseded(t.Context(), x.dir, "e", x.srv.URL, fresh, x.records(t))
	assert.Empty(t, rep.Retired)
	assert.Len(t, rep.Kept, 2)
	assert.Len(t, x.f.DeleteRequests(), before, "no delete is attempted on a guess")
	assert.Len(t, x.records(t), 2)
	require.Len(t, rep.Warnings, 2)
	assert.Contains(t, rep.Warnings[0], "user-b")
	assert.Contains(t, rep.Warnings[0], "different user")
	assert.Contains(t, rep.Warnings[0], "rancher auth login "+clierr.ShellQuote(clierr.RecoveryEntryName(x.srv.URL)))
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
	rep := RetireSuperseded(t.Context(), x.dir, "e", x.srv.URL, fresh, x.records(t))
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

// TestRetireV3ForbiddenKeepsTheRecord asserts the unproven v3 rule ships
// conservatively: a standard user's delete of their own v3 token through
// V3TokensPath answering 403 is failed, the record is kept and stamped,
// and the warning carries the management-token recovery sequence.
func TestRetireV3ForbiddenKeepsTheRecord(t *testing.T) {
	x := newRetireFixture(t, testrancher.Options{Legacy: true, LegacyLoginBodyUserID: true, V3DeleteForbiddenForNonAdmin: true})
	old, fresh := x.credAs(t, "a"), x.credAs(t, "a") // legacy: both are v3 session tokens with owner user-a
	require.Equal(t, TokenKindV3, old.Ref.Kind)
	x.seedRecords(t, []config.SupersededToken{record(old, "2026-09-02T00:00:00Z")})
	rep := RetireSuperseded(t.Context(), x.dir, "e", x.srv.URL, fresh, x.records(t))
	assert.Equal(t, []TokenRef{*old.Ref}, rep.Failed)
	recs := x.records(t)
	require.Len(t, recs, 1)
	require.NotNil(t, recs[0].LastAttempt)
	assert.Equal(t, clierr.ResultFailed, recs[0].LastAttempt.Result)
	assert.Contains(t, recs[0].LastAttempt.Error, "403")
	require.Len(t, rep.Warnings, 1)
	assert.Contains(t, rep.Warnings[0], "--allow-deprecated")
	_, alive := x.f.TokenOwner(old.Ref.Name)
	assert.True(t, alive)
}

// TestRetireOwnersABCThenA asserts A's record is retired and B's and C's
// are kept when A logs in again.
func TestRetireOwnersABCThenA(t *testing.T) {
	x := newRetireFixture(t, testrancher.Options{})
	a1, b, c, a2 := x.credAs(t, "a"), x.credAs(t, "b"), x.credAs(t, "c"), x.credAs(t, "a")
	x.seedRecords(t, []config.SupersededToken{record(a1, "2026-09-01T00:00:00Z"), record(b, "2026-09-02T00:00:00Z"), record(c, "2026-09-03T00:00:00Z")})
	rep := RetireSuperseded(t.Context(), x.dir, "e", x.srv.URL, a2, x.records(t))
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
	restore := setRetirementBudgetForTest(400 * time.Millisecond)
	t.Cleanup(restore)
	x := newRetireFixture(t, testrancher.Options{})
	r1, r2, r3, fresh := x.credAs(t, "a"), x.credAs(t, "a"), x.credAs(t, "a"), x.credAs(t, "a")
	for _, c := range []LoginCredential{r1, r2, r3} {
		x.f.SetDeleteMode(c.Ref.Name, "hang")
	}
	x.seedRecords(t, []config.SupersededToken{record(r1, "2026-09-01T00:00:00Z"), record(r2, "2026-09-02T00:00:00Z"), record(r3, "2026-09-03T00:00:00Z")})
	start := time.Now()
	rep := RetireSuperseded(t.Context(), x.dir, "e", x.srv.URL, fresh, x.records(t)) // t.Context carries no deadline: the deadline-less caller
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.Len(t, rep.Failed, 1)
	assert.Len(t, rep.Kept, 2, "unreached records are kept")
	recs := x.records(t)
	require.Len(t, recs, 3)
	assert.NotNil(t, recs[0].LastAttempt)
	assert.Nil(t, recs[1].LastAttempt, "unreached records are unstamped")
	assert.Nil(t, recs[2].LastAttempt)
	assert.Contains(t, rep.Warnings[len(rep.Warnings)-1], "not attempted")
}

// TestRetireRecordWriteLockTimeoutLeavesRecordsAndSucceeds asserts a
// contended config lock at the record write is a warning: the deletes
// happened, the records stay, the pass returns normally.
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
	rep := RetireSuperseded(t.Context(), x.dir, "e", x.srv.URL, fresh, x.records(t))
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
	rep := RetireSuperseded(t.Context(), x.dir, "e", x.srv.URL, fresh, x.records(t))
	assert.Equal(t, int32(0), violations.Load())
	assert.Len(t, rep.Retired, 1)
	cfg, _ := config.Load(config.FilePath(x.dir))
	assert.Contains(t, cfg.Servers, "sentinel", "the record write reloads under the lock, never clobbers")
	assert.Empty(t, cfg.Servers["e"].SupersededTokens)
}
