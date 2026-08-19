package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// seedLogout writes a config with the given current server and per-server
// tokens. Returns dir + store.
func seedLogout(t *testing.T, current string, servers ...string) (string, config.CredStore) {
	t.Helper()
	keyring.MockInit() // in-memory keychain: DeleteTokenEverywhere now deletes both backends unconditionally
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{
		CurrentServer: current,
		Servers:       map[string]config.Server{},
	}
	for _, s := range servers {
		cfg.Servers[s] = config.Server{URL: "https://" + s + ".ex"}
		require.NoError(t, store.SetToken(config.ServerRef{Name: s}, "id:sec"))
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir, store
}

func TestLogoutNoArgLogsOutCurrent(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme", "other")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "LogoutResult", res.Kind)

	data, isMap := res.Data.(map[string]any)
	require.True(t, isMap, "Result data is a map")
	df, isDF := data["deletedFrom"].(map[string][]string)
	require.True(t, isDF, "Result carries per-server deletedFrom")
	assert.Equal(t, []string{"file"}, df["acme"], "mock keychain is empty, so only the file backend reports a delete")

	_, err = store.GetToken(config.ServerRef{Name: "acme"})
	require.ErrorIs(t, err, config.ErrNoToken)
	_, err = store.GetToken(config.ServerRef{Name: "other"})
	require.NoError(t, err) // untouched

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Empty(t, cfg.CurrentServer) // unset
	_, ok := cfg.Servers["acme"]
	assert.True(t, ok, "server config entry kept") // token-only removal
}

func TestLogoutByName(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme", "other")
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{a: []string{"other"}}, ui.Discard())
	require.NoError(t, err)
	_, err = store.GetToken(config.ServerRef{Name: "other"})
	require.ErrorIs(t, err, config.ErrNoToken)
	_, err = store.GetToken(config.ServerRef{Name: "acme"})
	require.NoError(t, err) // current untouched
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "acme", cfg.CurrentServer) // not unset (other != current)
}

func TestLogoutUnknownNameIsNotFoundWithCandidates(t *testing.T) {
	dir, _ := seedLogout(t, "acme", "acme")
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{a: []string{"nope"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	ids := make([]string, 0, len(ce.Candidates))
	for _, c := range ce.Candidates {
		ids = append(ids, c.ID)
	}
	assert.Contains(t, ids, "acme")
}

func TestLogoutAll(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme", "other")
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	_, err = store.GetToken(config.ServerRef{Name: "acme"})
	require.ErrorIs(t, err, config.ErrNoToken)
	_, err = store.GetToken(config.ServerRef{Name: "other"})
	require.ErrorIs(t, err, config.ErrNoToken)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "", cfg.CurrentServer)
}

func TestLogoutDeletesMatchingExecCache(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme", "other")
	// acme.ex and other.ex per seedLogout; cache two entries, one per host,
	// plus a legacy bare entry (no Server) that must never be touched.
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), "tok-acme", time.Time{}, "kubeuser", "acme.ex", "c-1"))
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "other.ex", "c-1"), "tok-other", time.Time{}, "kubeuser", "other.ex", "c-1"))
	require.NoError(t, store.SetCachedCred("legacy-bare", "tok-legacy", time.Time{}, "", "", ""))

	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{a: []string{"acme"}}, ui.Discard())
	require.NoError(t, err)

	data := res.Data.(map[string]any)
	deletedCache, ok := data["deletedCache"].([]string)
	require.True(t, ok, "Result carries deletedCache")
	assert.Equal(t, []string{config.CacheKey("kubeuser", "acme.ex", "c-1")}, deletedCache)

	_, _, err = store.GetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"))
	require.ErrorIs(t, err, config.ErrNoToken, "matching entry deleted")
	_, _, err = store.GetCachedCred(config.CacheKey("kubeuser", "other.ex", "c-1"))
	require.NoError(t, err, "non-matching entry kept")
	_, _, err = store.GetCachedCred("legacy-bare")
	require.NoError(t, err, "legacy bare entry (no Server) kept")
}

func TestLogoutAllDeletesWholeExecCache(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme", "other")
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), "tok-acme", time.Time{}, "kubeuser", "acme.ex", "c-1"))
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "other.ex", "c-1"), "tok-other", time.Time{}, "kubeuser", "other.ex", "c-1"))

	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)

	data := res.Data.(map[string]any)
	deletedCache, ok := data["deletedCache"].([]string)
	require.True(t, ok, "Result carries deletedCache")
	assert.ElementsMatch(t, []string{
		config.CacheKey("kubeuser", "acme.ex", "c-1"),
		config.CacheKey("kubeuser", "other.ex", "c-1"),
	}, deletedCache)

	creds, err := store.ListCachedCreds()
	require.NoError(t, err)
	assert.Empty(t, creds, "--all clears the whole cache")
}

// Round-3 repro: --all's per-server host-scoped delete (round 2) missed a
// legacy bare entry (no Server field) and an entry cached against a server
// no longer in config.yaml — neither is reachable by any per-server delete.
// --all must catch both via the full-cache wipe.
func TestLogoutAllCatchesOrphanedCacheEntries(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme")
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), "tok-acme", time.Time{}, "kubeuser", "acme.ex", "c-1"))
	// Orphaned: no Server field at all (legacy bare entry).
	require.NoError(t, store.SetCachedCred("legacy-bare", "tok-legacy", time.Time{}, "", "", ""))
	// Orphaned: cached against a host with no matching entry in cfg.Servers.
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "removed.ex", "c-1"), "tok-removed", time.Time{}, "kubeuser", "removed.ex", "c-1"))

	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)

	data := res.Data.(map[string]any)
	deletedCache, ok := data["deletedCache"].([]string)
	require.True(t, ok)
	assert.ElementsMatch(t, []string{
		config.CacheKey("kubeuser", "acme.ex", "c-1"),
		"legacy-bare",
		config.CacheKey("kubeuser", "removed.ex", "c-1"),
	}, deletedCache, "--all must catch orphaned entries no per-server delete can reach")

	creds, err := store.ListCachedCreds()
	require.NoError(t, err)
	assert.Empty(t, creds, "no orphaned live tokens after --all")
}

func TestLogoutNameAndAllIsValidation(t *testing.T) {
	dir, _ := seedLogout(t, "acme", "acme")
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(),
		args{a: []string{"acme"}, b: map[string]bool{"all": true}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLogoutNoCurrentServer(t *testing.T) {
	dir, _ := seedLogout(t, "", "acme") // no current server set
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
}

func TestLogoutCurrentServerMissing(t *testing.T) {
	dir, _ := seedLogout(t, "ghost", "acme") // CurrentServer points at a name not in Servers
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
}

func TestLogoutCurrentServerNotInServersMap(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "acme",
		Servers:       map[string]config.Server{}, // server missing
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
}

func TestLogoutExecPolicyReportsHelper(t *testing.T) {
	dir, statePath := seedExecLogout(t, "acme", "acme")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)
	df := res.Data.(map[string]any)["deletedFrom"].(map[string][]string)
	assert.Contains(t, df["acme"], "helper:fake")
	assert.Empty(t, helperCreds(t, statePath), "helper-held credential erased")
}

func TestLogoutAllExecPolicyErasesEveryServer(t *testing.T) {
	dir, statePath := seedExecLogout(t, "acme", "acme", "other")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(),
		args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	df := res.Data.(map[string]any)["deletedFrom"].(map[string][]string)
	assert.Contains(t, df["acme"], "helper:fake")
	assert.Contains(t, df["other"], "helper:fake")
	assert.Empty(t, helperCreds(t, statePath), "--all erases every helper-held credential")
}

// TestLogoutAllDeletesOrphanedHelperCredential is the impl-review
// finding-2 repro: a helper credential recorded for a server
// name absent from config.yaml (left behind by a crash/failed write) is unreachable
// from the per-server logout loop, so `logout --all` must delete it via the
// orphaned-record path and report it.
func TestLogoutAllDeletesOrphanedHelperCredential(t *testing.T) {
	t.Setenv("PATH", buildCommandsFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEHELPER_MODE", "ok")
	statePath := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("FAKEHELPER_STATE", statePath)
	keyring.MockInit()

	dir := t.TempDir()
	cfg := &config.Config{
		CredentialStore: "exec:fake",
		Servers:         map[string]config.Server{},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	// Store a helper credential + its record for a name that never made it
	// into config.yaml.
	store, err := config.OpenStore("exec:fake", dir)
	require.NoError(t, err)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "ghost", URL: "https://ghost.ex"}, "id:sec"))
	require.NotEmpty(t, helperCreds(t, statePath), "precondition: helper holds the orphaned credential")

	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(),
		args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)

	df := res.Data.(map[string]any)["deletedFrom"].(map[string][]string)
	assert.Contains(t, df["ghost"], "helper:fake", "logout --all must report the deleted orphan under its server name")
	assert.Empty(t, helperCreds(t, statePath), "the orphaned helper credential must be erased")

	leftovers, err := config.DeleteOrphanedHelperCredentials(t.Context(), dir)
	require.NoError(t, err)
	assert.Empty(t, leftovers, "helper records must be fully deleted")
}

// TestLogoutAllDeletesLeftoversAfterFailedConfigWrite is finding-2's review-
// requested scenario: a login whose helper store succeeds but whose config
// write then fails strands a live helper credential + helper record for a
// server never persisted to config.yaml. `logout --all` must still clean it.
func TestLogoutAllDeletesLeftoversAfterFailedConfigWrite(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("symlink-based config-write failure needs POSIX symlink semantics")
	}
	t.Setenv("PATH", buildCommandsFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEHELPER_MODE", "ok")
	statePath := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("FAKEHELPER_STATE", statePath)
	keyring.MockInit()

	dir := t.TempDir()
	// A real config behind a symlink at the config path: config.Load follows
	// it fine, but atomicWriteFile refuses to write THROUGH a symlink — so
	// cfg.Write fails deterministically while the provenance write-ahead (a
	// different, non-symlink file) still succeeds.
	real := filepath.Join(dir, "config.real.yaml")
	realCfg := &config.Config{CredentialStore: "exec:fake", Servers: map[string]config.Server{}}
	realCfg.SetPath(real)
	require.NoError(t, realCfg.Write())
	require.NoError(t, os.Symlink(real, config.FilePath(dir)))

	store, err := config.OpenStore("exec:fake", dir)
	require.NoError(t, err)

	_, err = newLoginCmd(dir, store).Execute(t.Context(),
		args{s: map[string]string{"server": "https://ghost.ex", "name": "ghost"}, b: map[string]bool{"token-stdin": true}},
		stdinPrompter("id:secret"))
	require.Error(t, err, "the config write must fail (symlink refusal)")
	require.NotEmpty(t, helperCreds(t, statePath), "the helper stored the credential before the config write failed")

	// logout --all reads the unchanged config (ghost is absent from it) yet
	// must still delete the stranded helper credential via provenance.
	_, err = NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(),
		args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	assert.Empty(t, helperCreds(t, statePath), "logout --all must clean the stranded helper records")

	leftovers, err := config.DeleteOrphanedHelperCredentials(t.Context(), dir)
	require.NoError(t, err)
	assert.Empty(t, leftovers)
}

func TestLogoutAllClearsDanglingCurrentServer(t *testing.T) {
	keyring.MockInit()
	// CurrentServer points at a name not present in Servers (dangling pointer).
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{
		CurrentServer: "ghost",
		Servers:       map[string]config.Server{"acme": {URL: "https://acme.ex"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme"}, "id:sec"))

	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	cfgAfter, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Equal(t, "", cfgAfter.CurrentServer)
}

// TestDeleteFailureCancellationPassesThrough pins the fourth-review P1: a
// delete interrupted by the caller's own cancellation must surface as
// context.Canceled through logout's failure shaping — never rewrapped into
// the generic "failed to log out" clierr, which severs the errors.Is chain
// and reads as a helper fault.
func TestDeleteFailureCancellationPassesThrough(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("locking the credential-helper records: %w", context.Canceled)
	err := deleteFailure("failed to log out \"prod\"", wrapped, []string{"a"})
	require.ErrorIs(t, err, context.Canceled)
	var ce *clierr.Error
	assert.NotErrorAs(t, err, &ce, "cancellation must not be dressed as a clierr delete failure")

	real := deleteFailure("failed to log out \"prod\"", fmt.Errorf("helper exploded"), []string{"a"})
	require.ErrorAs(t, real, &ce)
	assert.Contains(t, ce.Hint, "already logged out")
}

// TestDeleteFailurePreservesLockTimeout pins the exercise-8/9-fixes review
// finding: a LOCK_TIMEOUT surfacing from the token-cache or helper-records
// lock inside the delete must keep its code and retryability through logout's
// failure shaping — flattening it to GENERIC hides exit 8 and the retry
// signal from scripted callers.
func TestDeleteFailurePreservesLockTimeout(t *testing.T) {
	t.Parallel()
	inner := clierr.Retryable(clierr.CodeLockTimeout,
		"locking token-cache.yaml (another rancher process may be writing): context deadline exceeded")
	err := deleteFailure("failed to log out \"prod\"", inner, []string{"a"})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeLockTimeout, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Equal(t, 8, ce.ExitCode())
	assert.Contains(t, ce.Message, "failed to log out")
	assert.Contains(t, ce.Hint, "already logged out")
}

// TestLogoutTextAndCurrentServerClearedReported verifies the TTY story: logout
// is no longer silent — the text names the server, its URL, the backends the
// token was removed from, and that the current server was cleared — and the
// envelope carries currentServerCleared for programs.
func TestLogoutTextAndCurrentServerClearedReported(t *testing.T) {
	dir, _ := seedLogout(t, "acme", "acme", "other")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)

	data := res.Data.(map[string]any)
	assert.Equal(t, true, data["currentServerCleared"])

	assert.Contains(t, res.Text, "logged out of acme (https://acme.ex)")
	assert.Contains(t, res.Text, "token removed from: file")
	assert.Contains(t, res.Text, "current server cleared")
}

// TestLogoutTextNoCurrentServerClearWhenOtherServer makes sure logging out a
// server other than the current one says nothing about the current server
// pointer — it was not touched.
func TestLogoutTextNoCurrentServerClearWhenOtherServer(t *testing.T) {
	dir, _ := seedLogout(t, "acme", "acme", "other")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{a: []string{"other"}}, ui.Discard())
	require.NoError(t, err)

	data := res.Data.(map[string]any)
	assert.Equal(t, false, data["currentServerCleared"])
	assert.Contains(t, res.Text, "logged out of other (https://other.ex)")
	assert.NotContains(t, res.Text, "current server cleared")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "acme", cfg.CurrentServer, "current server untouched")
}
