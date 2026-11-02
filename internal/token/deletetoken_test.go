package token

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deleteTokenData is auth delete-token's data as the JSON envelope carries
// it.
type deleteTokenData struct {
	Deleted    []string          `json:"deleted"`
	Kept       []string          `json:"kept"`
	Revoked    []RevokedToken    `json:"revoked"`
	NotRevoked []NotRevokedToken `json:"notRevoked"`
}

func decodeDeleteToken(t *testing.T, res runtime.Result) (deleteTokenData, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var data deleteTokenData
	require.NoError(t, json.Unmarshal(raw, &data))
	var generic map[string]any
	require.NoError(t, json.Unmarshal(raw, &generic))
	return data, generic
}

// TestDeleteTokenAllReportsWhatItDeletedKeptAndRevoked asserts --all's
// data - deleted as the list of keys, kept the record the plugin rewrote
// meanwhile, both revocation lists - its warning for the kept record, and
// its text: the count, then one line per kept record, then the tokens.
func TestDeleteTokenAllReportsWhatItDeletedKeptAndRevoked(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	one, two, fresh := x.token("alice"), x.token("alice"), x.token("alice")
	x.cache("one", one, x.own("prod"), true)
	x.cache("two", two, x.own("prod"), true)
	store := rewriteBetweenHolds{CredStore: x.store, rewrite: func() { x.cache("two", fresh, x.own("prod"), true) }}
	res, err := NewDeleteTokenCommand(x.dir, store).(runtime.ReadCommand).Execute(t.Context(), tin{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	data, generic := decodeDeleteToken(t, res)
	assert.Equal(t, []string{"one"}, data.Deleted)
	assert.Equal(t, []string{"two"}, data.Kept)
	assert.Equal(t, []any{}, generic["notRevoked"])
	assert.ElementsMatch(t, []string{one.Ref.Name, two.Ref.Name}, revokedNames(RevocationReport{Revoked: data.Revoked}))
	assert.True(t, x.alive(fresh), "the token the plugin wrote meanwhile is not touched")
	assert.Contains(t, res.Warnings, "the cached credential two was kept: the exec plugin wrote a new token to it while it was being deleted")
	lines := strings.Split(strings.TrimSuffix(res.Text, "\n"), "\n")
	assert.Equal(t, "deleted 1 cached credentials", lines[0])
	assert.Equal(t, "cached credential two kept: the exec plugin wrote a new token to it meanwhile", lines[1])
	assert.Equal(t, "tokens on "+config.CanonicalServerIdentity(x.url)+":", lines[2])
	assert.Empty(t, x.load().Servers["prod"].SupersededTokens, "confirmed revokes are settled")
}

// TestDeleteTokenByNameRecordsAndRevokes asserts a named delete: only the
// records with that name or key, their tokens revoked with their own
// bearer, nothing left recorded.
func TestDeleteTokenByNameRecordsAndRevokes(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	mine, other := x.token("alice"), x.token("alice")
	x.cache("mine", mine, x.own("prod"), true)
	x.cache("other", other, x.own("prod"), true)
	before := len(x.f.DeleteRequests())
	res, err := NewDeleteTokenCommand(x.dir, x.store).(runtime.ReadCommand).Execute(t.Context(), tin{a: []string{"mine"}}, ui.Discard())
	require.NoError(t, err)
	data, _ := decodeDeleteToken(t, res)
	assert.Equal(t, []string{"mine"}, data.Deleted)
	assert.Equal(t, []string{}, data.Kept)
	require.Len(t, data.Revoked, 1)
	assert.Equal(t, mine.Ref.Name, data.Revoked[0].Ref.Name)
	deletes := x.f.DeleteRequests()[before:]
	require.Len(t, deletes, 1)
	assert.Equal(t, mine.Token, deletes[0].Bearer)
	assert.Equal(t, []string{"other"}, x.cacheKeys())
	assert.True(t, x.alive(other))
}

// TestDeleteTokenRefusesAnUnparseableConfigBeforeAnyDelete asserts that a
// config.yaml that cannot be parsed fails the command with nothing deleted.
func TestDeleteTokenRefusesAnUnparseableConfigBeforeAnyDelete(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	x.cache("k", x.token("alice"), x.own("prod"), true)
	require.NoError(t, os.WriteFile(config.FilePath(x.dir), []byte("\t: not yaml ["), 0o600))
	_, err := NewDeleteTokenCommand(x.dir, x.store).(runtime.ReadCommand).Execute(t.Context(), tin{b: map[string]bool{"all": true}}, ui.Discard())
	require.Error(t, err)
	assert.Equal(t, []string{"k"}, x.cacheKeys())
}

// TestDeleteTokenWithTheConfigLockHeldDeletesNothing asserts that a config
// lock it cannot take returns the lock error with nothing deleted.
func TestDeleteTokenWithTheConfigLockHeldDeletesNothing(t *testing.T) {
	t.Cleanup(config.SetLockWaitTimeoutForTest(200 * time.Millisecond))
	x := newRevFixture(t, testrancher.Options{})
	x.login("prod", "alice")
	x.cache("k", x.token("alice"), x.own("prod"), true)
	held, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = config.WithLock(t.Context(), x.dir, func() error { close(held); <-release; return nil })
	})
	<-held
	_, err := NewDeleteTokenCommand(x.dir, x.store).(runtime.ReadCommand).Execute(t.Context(), tin{a: []string{"k"}}, ui.Discard())
	close(release)
	wg.Wait()
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeLockTimeout, ce.Code)
	assert.Equal(t, []string{"k"}, x.cacheKeys())
}

// TestDeleteTokenRevokesNothingUnderTheConfigLock asserts no request runs
// while the config lock is held.
func TestDeleteTokenRevokesNothingUnderTheConfigLock(t *testing.T) {
	var dir string
	var violations, deletes atomic.Int32
	x := newRevFixture(t, testrancher.Options{RequestHook: func(r *http.Request) {
		if r.Method != http.MethodDelete || dir == "" {
			return
		}
		deletes.Add(1)
		if configLockHeld(dir) {
			violations.Add(1)
		}
	}})
	x.login("prod", "alice")
	x.cache("k", x.token("alice"), x.own("prod"), true)
	dir = x.dir
	_, err := NewDeleteTokenCommand(x.dir, x.store).(runtime.ReadCommand).Execute(t.Context(), tin{a: []string{"k"}}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, int32(1), deletes.Load())
	assert.Equal(t, int32(0), violations.Load())
}

// TestDeleteTokenAllRemovesAnUnreadableCacheAndSaysSo asserts that --all
// removes a cache file it cannot read, exits 0 with nothing listed, and
// warns that the tokens it held could be neither revoked nor recorded.
func TestDeleteTokenAllRemovesAnUnreadableCacheAndSaysSo(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	require.NoError(t, os.WriteFile(x.dir+"/token-cache.yaml", []byte("\t: not yaml ["), 0o600))
	res, err := NewDeleteTokenCommand(x.dir, x.store).(runtime.ReadCommand).Execute(t.Context(), tin{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	data, _ := decodeDeleteToken(t, res)
	assert.Equal(t, []string{}, data.Deleted)
	assert.Contains(t, res.Warnings, "token-cache.yaml could not be read; it was removed, and the tokens it held could not be revoked or recorded")
	_, serr := os.Stat(x.dir + "/token-cache.yaml")
	assert.True(t, os.IsNotExist(serr))
}

// TestDeleteTokenThatCannotTakeTheCacheLockDeletesNothing asserts that a
// token-cache lock auth delete-token cannot take fails it, by name and
// with --all, with the lock's own error before anything is written,
// deleted or revoked; --all never removes the file for it.
func TestDeleteTokenThatCannotTakeTheCacheLockDeletesNothing(t *testing.T) {
	for name, in := range map[string]tin{
		"by name": {a: []string{"k"}},
		"--all":   {b: map[string]bool{"all": true}},
	} {
		t.Run(name, func(t *testing.T) {
			x := newRevFixture(t, testrancher.Options{})
			x.login("prod", "alice")
			ct := x.token("alice")
			x.cache("k", ct, x.own("prod"), true)
			deletes := len(x.f.DeleteRequests())
			t.Cleanup(config.SetCacheLockWaitForTest(200 * time.Millisecond))
			fl := flock.New(filepath.Join(x.dir, "token-cache.lock"))
			require.NoError(t, fl.Lock())
			t.Cleanup(func() { _ = fl.Unlock() })

			_, err := NewDeleteTokenCommand(x.dir, x.store).(runtime.ReadCommand).Execute(t.Context(), in, ui.Discard())
			var ce *clierr.Error
			require.ErrorAs(t, err, &ce)
			assert.Equal(t, clierr.CodeLockTimeout, ce.Code)
			assert.Equal(t, []string{"k"}, x.cacheKeys(), "the record stays")
			assert.Len(t, x.f.DeleteRequests(), deletes, "nothing is revoked")
			assert.True(t, x.alive(ct))
			assert.Empty(t, x.load().Servers["prod"].SupersededTokens, "nothing is written")
		})
	}
}

// TestDeleteTokenByNameRefusesAnUnreadableCache asserts that a named delete
// on a token-cache.yaml it cannot parse returns the read error, deletes
// nothing and keeps the file.
func TestDeleteTokenByNameRefusesAnUnreadableCache(t *testing.T) {
	x := newRevFixture(t, testrancher.Options{})
	path := filepath.Join(x.dir, "token-cache.yaml")
	require.NoError(t, os.WriteFile(path, []byte("\t: not yaml ["), 0o600))
	_, err := NewDeleteTokenCommand(x.dir, x.store).(runtime.ReadCommand).Execute(t.Context(), tin{a: []string{"k"}}, ui.Discard())
	require.ErrorContains(t, err, "reading token-cache.yaml")
	b, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Equal(t, "\t: not yaml [", string(b), "the file is kept")
}

// TestDeleteTokenInterruptedDuringTheRevokesReportsBothLists asserts that a
// Ctrl-C during the revokes ends auth delete-token with INTERRUPTED, its
// details carrying the deleted keys and both lists.
func TestDeleteTokenInterruptedDuringTheRevokesReportsBothLists(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var armed atomic.Bool
	x := newRevFixture(t, testrancher.Options{RequestHook: func(r *http.Request) {
		if r.Method == http.MethodDelete && armed.CompareAndSwap(true, false) {
			cancel()
		}
	}})
	x.login("prod", "alice")
	one, two := x.token("alice"), x.token("alice")
	x.cache("one", one, x.own("prod"), true)
	x.cache("two", two, x.own("prod"), true)
	armed.Store(true)
	_, err := NewDeleteTokenCommand(x.dir, x.store).(runtime.ReadCommand).Execute(ctx, tin{b: map[string]bool{"all": true}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeInterrupted, ce.Code)
	details, ok := ce.Details.(DeleteTokenFailure)
	require.True(t, ok)
	assert.Equal(t, []string{"one", "two"}, details.Deleted)
	require.Len(t, details.Revoked, 1)
	require.Len(t, details.NotRevoked, 1)
	assert.Equal(t, "cancelled", details.NotRevoked[0].Reason)
	assert.Contains(t, strings.Join(ce.Warnings(), "\n"), "1 token(s) recorded on prod are revoked by the next login of prod", "the warnings carry what revokes the cancelled token")
}

// TestDeleteTokenWhoseSignalCameAfterTheLastRevokeEndsNormally asserts that
// a Ctrl-C landing during the last revoke, which completes, leaves no token
// unreached: auth delete-token returns its result, not INTERRUPTED.
func TestDeleteTokenWhoseSignalCameAfterTheLastRevokeEndsNormally(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var deletes atomic.Int32
	var armed atomic.Bool
	x := newRevFixture(t, testrancher.Options{RequestHook: func(r *http.Request) {
		if r.Method == http.MethodDelete && armed.Load() && deletes.Add(1) == 2 {
			cancel()
		}
	}})
	x.login("prod", "alice")
	x.cache("one", x.token("alice"), x.own("prod"), true)
	x.cache("two", x.token("alice"), x.own("prod"), true)
	armed.Store(true)
	res, err := NewDeleteTokenCommand(x.dir, x.store).(runtime.ReadCommand).Execute(ctx, tin{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	data, _ := decodeDeleteToken(t, res)
	assert.Len(t, data.Revoked, 2)
}
