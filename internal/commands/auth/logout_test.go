package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/commands/cmdtest"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output/style"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
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
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
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
	assert.Equal(t, "acme", cfg.CurrentServer, "logout never clears the pointer, even for the current server")
	_, ok := cfg.Servers["acme"]
	assert.True(t, ok, "server config entry kept") // token-only removal
}

func TestLogoutByName(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme", "other")
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"other"}}, ui.Discard())
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
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"nope"}}, ui.Discard())
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
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	_, err = store.GetToken(config.ServerRef{Name: "acme"})
	require.ErrorIs(t, err, config.ErrNoToken)
	_, err = store.GetToken(config.ServerRef{Name: "other"})
	require.ErrorIs(t, err, config.ErrNoToken)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "acme", cfg.CurrentServer, "logout --all no longer resets the pointer - a credential-less entry is a valid state")
}

// TestLogoutAllReachesAnEntryRemovedFromTheConfig asserts that `--all`
// enumerates the directory's credential identities on top of its configured
// servers: an entry deleted from config.yaml by hand still has a keychain
// credential, and its identity record is the only thing that still names
// it.
func TestLogoutAllReachesAnEntryRemovedFromTheConfig(t *testing.T) {
	dir, _ := seedLogout(t, "acme", "acme")
	keychain, err := config.OpenStore("keychain", dir)
	require.NoError(t, err)
	require.NoError(t, keychain.SetToken(config.ServerRef{Name: "gone", URL: "https://gone.ex"}, "id:sec"))

	_, err = NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)

	_, err = keychain.GetToken(config.ServerRef{Name: "gone"})
	assert.ErrorIs(t, err, config.ErrNoToken, "a credential whose config entry is gone is still logged out")
}

// TestLogoutAllOnEmptyConfigReturnsText asserts that logging out of every
// server when none are configured is still a reportable success: ShapeText
// requires non-empty Text on every default-format success, and a config
// with zero servers is the one path where deletedFrom's per-server loop
// produces nothing to report.
func TestLogoutAllOnEmptyConfigReturnsText(t *testing.T) {
	dir, _ := seedLogout(t, "")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	assert.NotEmpty(t, res.Text, "logout --all on an empty config must still produce operator-facing text")
}

func TestLogoutDeletesMatchingExecCache(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme", "other")
	// acme.ex and other.ex per seedLogout; cache two entries, one per host,
	// plus a legacy bare entry (no Server) that must never be touched.
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), "tok-acme", time.Time{}, "kubeuser", "acme.ex", "c-1", config.CacheBindingForTest(dir, "", "")))
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "other.ex", "c-1"), "tok-other", time.Time{}, "kubeuser", "other.ex", "c-1", config.CacheBindingForTest(dir, "", "")))
	require.NoError(t, store.SetCachedCred("legacy-bare", "tok-legacy", time.Time{}, "", "", "", config.CacheBindingForTest(dir, "", "")))

	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"acme"}}, ui.Discard())
	require.NoError(t, err)

	data := res.Data.(map[string]any)
	deletedCache, ok := data["deletedCache"].([]string)
	require.True(t, ok, "Result carries deletedCache")
	assert.Equal(t, []string{config.CacheKey("kubeuser", "acme.ex", "c-1")}, deletedCache)

	_, _, err = store.GetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), cmdtest.CacheAnchor(t, dir))
	require.ErrorIs(t, err, config.ErrNoToken, "matching entry deleted")
	_, _, err = store.GetCachedCred(config.CacheKey("kubeuser", "other.ex", "c-1"), cmdtest.CacheAnchor(t, dir))
	require.NoError(t, err, "non-matching entry kept")
	_, _, err = store.GetCachedCred("legacy-bare", cmdtest.CacheAnchor(t, dir))
	require.NoError(t, err, "legacy bare entry (no Server) kept")
}

func TestLogoutAllDeletesWholeExecCache(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme", "other")
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), "tok-acme", time.Time{}, "kubeuser", "acme.ex", "c-1", config.CacheBindingForTest(dir, "", "")))
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "other.ex", "c-1"), "tok-other", time.Time{}, "kubeuser", "other.ex", "c-1", config.CacheBindingForTest(dir, "", "")))

	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
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

// TestLogoutAllSurfacesExecCacheLoosePermissionNotice asserts the same drain
// end to end: `auth logout --all` deletes its exec-credential cache via
// config.DeleteAllExecCache, whose own throwaway *fileStore must drain its
// notice buffer so a loose-permission notice on token-cache.yaml reaches
// Result.Warnings instead of going out of scope with the throwaway store.
func TestLogoutAllSurfacesExecCacheLoosePermissionNotice(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("loose-permission notice is intentionally disabled on Windows; ACL enforcement is the guarantee there")
	}
	dir, store := seedLogout(t, "acme", "acme")
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), "tok-acme", time.Time{}, "kubeuser", "acme.ex", "c-1", config.CacheBindingForTest(dir, "", "")))
	require.NoError(t, os.Chmod(filepath.Join(dir, "token-cache.yaml"), 0o644))

	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	require.Len(t, res.Warnings, 1, "the exec-cache store's loose-permission notice must reach Result.Warnings")
	assert.Contains(t, res.Warnings[0], "group/world-readable")
}

// Round-3 repro: --all's per-server host-scoped delete (round 2) missed a
// legacy bare entry (no Server field) and an entry cached against a server
// no longer in config.yaml - neither is reachable by any per-server delete.
// --all must catch both via the full-cache wipe.
func TestLogoutAllCatchesOrphanedCacheEntries(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme")
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), "tok-acme", time.Time{}, "kubeuser", "acme.ex", "c-1", config.CacheBindingForTest(dir, "", "")))
	// Orphaned: no Server field at all (legacy bare entry).
	require.NoError(t, store.SetCachedCred("legacy-bare", "tok-legacy", time.Time{}, "", "", "", config.CacheBindingForTest(dir, "", "")))
	// Orphaned: cached against a host with no matching entry in cfg.Servers.
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "removed.ex", "c-1"), "tok-removed", time.Time{}, "kubeuser", "removed.ex", "c-1", config.CacheBindingForTest(dir, "", "")))

	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
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
		cmdtest.Input{Pos: []string{"acme"}, Flag: map[string]bool{"all": true}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLogoutNoCurrentServer(t *testing.T) {
	dir, _ := seedLogout(t, "", "acme") // no current server set
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
}

func TestLogoutCurrentServerMissing(t *testing.T) {
	dir, _ := seedLogout(t, "ghost", "acme") // CurrentServer points at a name not in Servers
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
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
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
}

func TestLogoutExecPolicyReportsHelper(t *testing.T) {
	dir, statePath := cmdtest.SeedExecLogout(t, "acme", "acme")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
	require.NoError(t, err)
	df := res.Data.(map[string]any)["deletedFrom"].(map[string][]string)
	assert.Contains(t, df["acme"], "helper:fake")
	assert.Empty(t, cmdtest.HelperCreds(t, statePath), "helper-held credential erased")
}

func TestLogoutAllExecPolicyErasesEveryServer(t *testing.T) {
	dir, statePath := cmdtest.SeedExecLogout(t, "acme", "acme", "other")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(),
		cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	df := res.Data.(map[string]any)["deletedFrom"].(map[string][]string)
	assert.Contains(t, df["acme"], "helper:fake")
	assert.Contains(t, df["other"], "helper:fake")
	assert.Empty(t, cmdtest.HelperCreds(t, statePath), "--all erases every helper-held credential")
}

// TestLogoutAllDeletesOrphanedHelperCredential is the impl-review
// finding-2 repro: a helper credential recorded for a server
// name absent from config.yaml (left behind by a crash/failed write) is unreachable
// from the per-server logout loop, so `logout --all` must delete it via the
// orphaned-record path and report it.
func TestLogoutAllDeletesOrphanedHelperCredential(t *testing.T) {
	t.Setenv("PATH", cmdtest.BuildFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
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
	require.NotEmpty(t, cmdtest.HelperCreds(t, statePath), "precondition: helper holds the orphaned credential")

	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(),
		cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)

	df := res.Data.(map[string]any)["deletedFrom"].(map[string][]string)
	assert.Contains(t, df["ghost"], "helper:fake", "logout --all must report the deleted orphan under its server name")
	assert.Empty(t, cmdtest.HelperCreds(t, statePath), "the orphaned helper credential must be erased")

	leftovers, err := config.DeleteOrphanedHelperCredentials(t.Context(), dir)
	require.NoError(t, err)
	assert.Empty(t, leftovers, "helper records must be fully deleted")
}

// TestLogoutAllSweepsOrphansWhenTheCacheWipeFails asserts the best-effort
// contract between the two cleanups: an unreadable token-cache.yaml fails
// the cache wipe, and the orphaned-helper sweep - the only path to a
// credential whose server name never reached config.yaml - still runs and
// still erases it. The failure names the step that failed and carries
// what the other one deleted.
func TestLogoutAllSweepsOrphansWhenTheCacheWipeFails(t *testing.T) {
	t.Setenv("PATH", cmdtest.BuildFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
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

	store, err := config.OpenStore("exec:fake", dir)
	require.NoError(t, err)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "ghost", URL: "https://ghost.ex"}, "id:sec"))
	require.NotEmpty(t, cmdtest.HelperCreds(t, statePath), "precondition: the helper holds the orphaned credential")

	// A cache file the wipe cannot read, for a reason that has nothing to
	// do with the helper credential.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "token-cache.yaml"), []byte("\t: not yaml ["), 0o600))

	_, err = NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(),
		cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to delete exec-credential cache")
	require.NotContains(t, err.Error(), "and orphaned helper credentials",
		"one failing step still reports in its own shape")

	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	details, ok := ce.Details.(LogoutAllPartialFailure)
	require.True(t, ok, "the error must carry what already succeeded")
	assert.Contains(t, details.DeletedFrom["ghost"], "helper:fake",
		"the sweep ran despite the cache failure and reported its deletion")
	assert.Empty(t, details.DeletedCache, "the wipe deleted nothing")

	assert.Empty(t, cmdtest.HelperCreds(t, statePath),
		"the orphaned helper credential must not survive an unrelated cache failure")
}

// TestLogoutAllReportsBothCleanupFailures asserts what a run where both
// cleanups fail says: one failure naming both steps, with each cause still
// reachable through the error, and everything the per-target loop deleted
// still in the payload.
func TestLogoutAllReportsBothCleanupFailures(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("symlink-based write failure needs POSIX symlink semantics")
	}
	t.Setenv("PATH", cmdtest.BuildFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEHELPER_MODE", "ok")
	statePath := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("FAKEHELPER_STATE", statePath)
	keyring.MockInit()

	dir := t.TempDir()
	cfg := &config.Config{
		CredentialStore: "file",
		Servers:         map[string]config.Server{"acme": {URL: "https://acme.ex"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	fileStore := config.NewFileStore(dir)
	require.NoError(t, fileStore.SetToken(config.ServerRef{Name: "acme", URL: "https://acme.ex"}, "id:sec"))

	execStore, err := config.OpenStore("exec:fake", dir)
	require.NoError(t, err)
	require.NoError(t, execStore.SetToken(config.ServerRef{Name: "ghost", URL: "https://ghost.ex"}, "id:sec"))

	// The sweep's persist write is refused behind a symlink, the same way
	// its own test arranges it.
	recordsPath := filepath.Join(dir, "credhelper-provenance.yaml")
	data, rerr := os.ReadFile(recordsPath)
	require.NoError(t, rerr)
	backing := filepath.Join(dir, "credhelper-provenance.real.yaml")
	require.NoError(t, os.WriteFile(backing, data, 0o600))
	require.NoError(t, os.Remove(recordsPath))
	require.NoError(t, os.Symlink(backing, recordsPath))

	// And the cache wipe cannot read its file.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "token-cache.yaml"), []byte("\t: not yaml ["), 0o600))

	_, err = NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(),
		cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to delete exec-credential cache (")
	require.ErrorContains(t, err, "and orphaned helper credentials (")
	require.NotContains(t, err.Error(), "\n", "the two causes read as one line")

	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	joined, ok := ce.Unwrap().(interface{ Unwrap() []error })
	require.True(t, ok, "both causes ride the error rather than one replacing the other")
	causes := joined.Unwrap()
	require.Len(t, causes, 2)
	for _, cause := range causes {
		require.ErrorIs(t, err, cause, "errors.Is still reaches each cause")
	}

	details, ok := ce.Details.(LogoutAllPartialFailure)
	require.True(t, ok)
	assert.Equal(t, []string{"acme"}, details.LoggedOut)
	assert.Contains(t, details.DeletedFrom["acme"], "file",
		"what the per-target loop deleted is still reported")

	_, gerr := fileStore.GetToken(config.ServerRef{Name: "acme", URL: "https://acme.ex"})
	require.ErrorIs(t, gerr, config.ErrNoToken)
	after, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.False(t, after.Servers["acme"].Paused(), "the last config write, before the wipe and the sweep, resumed acme's cache writes")
}

// TestDeleteBothFailureKeepsBothCauses asserts what a run where both
// cleanups failed reports, in the shape no end-to-end fixture reaches: a
// classified cause beside a plain one. Neither cause's code or retry flag
// is adopted - inheriting the cache lock's retryable LOCK_TIMEOUT would
// tell a caller to retry a corruption that will never clear - and both
// stay matchable through the error.
func TestDeleteBothFailureKeepsBothCauses(t *testing.T) {
	t.Parallel()
	cacheErr := clierr.Retryable(clierr.CodeLockTimeout, "token-cache.lock is held by another process")
	sweepErr := errors.New("credential-helper records file is corrupt")

	err := deleteBothFailure(cacheErr, sweepErr, []string{"acme"},
		map[string][]string{"acme": {"file"}}, nil, []string{"a notice"})

	require.ErrorIs(t, err, cacheErr)
	require.ErrorIs(t, err, sweepErr)

	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeGeneric, ce.Code, "neither cause's classification is adopted")
	assert.False(t, ce.Retryable)
	assert.Contains(t, ce.Message, "token-cache.lock is held by another process")
	assert.Contains(t, ce.Message, "credential-helper records file is corrupt")
	assert.NotContains(t, ce.Message, "\n", "the message stays one line for the text renderer")

	details, ok := ce.Details.(LogoutAllPartialFailure)
	require.True(t, ok)
	assert.Equal(t, []string{"acme"}, details.LoggedOut)
	assert.Equal(t, []string{"a notice"}, ce.Warnings())
}

// TestDeleteBothFailureReportsACancellationAsOne asserts that a cancelled
// step is reported as the cancellation it is, whichever of the two
// cleanups was cancelled, rather than as a cleanup failure that happens
// to contain one: the command was interrupted, and that is what the
// caller has to see.
func TestDeleteBothFailureReportsACancellationAsOne(t *testing.T) {
	t.Parallel()
	other := errors.New("token-cache.yaml is corrupt")

	for _, tc := range []struct {
		name               string
		cacheErr, sweepErr error
	}{
		{"the cache wipe was cancelled", context.Canceled, other},
		{"the sweep was cancelled", other, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := deleteBothFailure(tc.cacheErr, tc.sweepErr, nil, nil, nil, nil)
			require.ErrorIs(t, err, context.Canceled)

			_, dressed := errors.AsType[*clierr.Error](err)
			assert.False(t, dressed,
				"a cancellation passes through undressed, as it does from every other step here")
		})
	}
}

// TestLogoutAllOrphanSweepFailureReportsWhatAlreadySucceeded asserts that a
// failure in the LAST of deleteAll's three steps must not discard the
// first two: the per-server loop and exec-cache wipe both succeed before
// the orphan sweep runs, so their results must survive a sweep failure,
// not be vaporized. "acme" is a plain file-store entry (its deletion never touches
// the helper-records file at all, exec policy or not); "ghost" is an
// orphaned EXEC-helper record, opened directly against the "exec:fake"
// backend independent of cfg's own "file" policy - matching how a real
// orphan arises (left behind by a PAST exec policy, swept regardless of
// the CURRENT one). Only the orphan sweep's persist write is refused.
func TestLogoutAllOrphanSweepFailureReportsWhatAlreadySucceeded(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("symlink-based write failure needs POSIX symlink semantics")
	}
	t.Setenv("PATH", cmdtest.BuildFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEHELPER_MODE", "ok")
	statePath := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("FAKEHELPER_STATE", statePath)
	keyring.MockInit()

	dir := t.TempDir()
	cfg := &config.Config{
		CredentialStore: "file",
		Servers:         map[string]config.Server{"acme": {URL: "https://acme.ex"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	fileStore := config.NewFileStore(dir)
	require.NoError(t, fileStore.SetToken(config.ServerRef{Name: "acme", URL: "https://acme.ex"}, "id:sec"))

	// A helper credential for a name never written to config.yaml - the
	// orphan the sweep step exists to reach - opened directly against
	// exec:fake regardless of cfg's "file" policy above.
	execStore, err := config.OpenStore("exec:fake", dir)
	require.NoError(t, err)
	require.NoError(t, execStore.SetToken(config.ServerRef{Name: "ghost", URL: "https://ghost.ex"}, "id:sec"))

	// credhelper-provenance.yaml (internal/config's helperRecordsFileName)
	// behind a symlink: reads during the sweep still resolve, but its
	// persist write is refused - the per-server loop (file-backed "acme")
	// and exec-cache wipe never touch this file, so they complete for real
	// first.
	recordsPath := filepath.Join(dir, "credhelper-provenance.yaml")
	data, rerr := os.ReadFile(recordsPath)
	require.NoError(t, rerr, "precondition: the helper-records file exists after ghost's SetToken above")
	backing := filepath.Join(dir, "credhelper-provenance.real.yaml")
	require.NoError(t, os.WriteFile(backing, data, 0o600))
	require.NoError(t, os.Remove(recordsPath))
	require.NoError(t, os.Symlink(backing, recordsPath))

	_, err = NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(),
		cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
	require.Error(t, err, "the orphan sweep's persist write must fail (symlink refusal)")

	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	details, ok := ce.Details.(LogoutAllPartialFailure)
	require.True(t, ok, "the error must carry LogoutAllPartialFailure, not discard what already succeeded")
	assert.Equal(t, []string{"acme"}, details.LoggedOut)
	assert.Contains(t, details.DeletedFrom["acme"], "file", "acme's credential was really deleted before the failing step")
	assert.NotNil(t, details.DeletedCache, "the exec-cache wipe completed before the failing step")

	lines := details.TextDetails()
	require.NotEmpty(t, lines)
	assert.Equal(t, "already logged out: acme", lines[0])
	assert.Contains(t, lines, "credentials removed: acme: file")
	assert.Equal(t, "those credentials are gone regardless of this failure - re-run auth logout --all to finish",
		lines[len(lines)-1])

	_, gerr := fileStore.GetToken(config.ServerRef{Name: "acme", URL: "https://acme.ex"})
	require.ErrorIs(t, gerr, config.ErrNoToken, "acme's credential deletion is not undone by the later failure")
	after, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	assert.False(t, after.Servers["acme"].Paused(), "the last config write, before the sweep, resumed acme's cache writes")
}

// TestDelegatedLoginConfigWriteFailureNeverStrandsHelperCredential asserts a
// safety property the server-registration restructure adds: registration
// (config.yaml) commits BEFORE any authentication or credential write, in
// its own lock cycle. A registration-time config-write failure must
// therefore leave no helper credential behind at all - the interleaving
// (helper store succeeds, then config write fails) that used to strand a
// credential is now structurally impossible, because delegated login can
// no longer reach the credential-write step without registration's own
// cfg.Write() having already succeeded. TestLogoutAllDeletesOrphanedHelperCredential
// covers `logout --all`'s orphan sweep directly, independent of login.
func TestDelegatedLoginConfigWriteFailureNeverStrandsHelperCredential(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("symlink-based config-write failure needs POSIX symlink semantics")
	}
	t.Setenv("PATH", cmdtest.BuildFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEHELPER_MODE", "ok")
	statePath := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("FAKEHELPER_STATE", statePath)
	keyring.MockInit()

	dir := t.TempDir()
	// A real config behind a symlink at the config path: config.Load follows
	// it fine, but atomicWriteFile refuses to write THROUGH a symlink - so
	// cfg.Write fails deterministically inside commitCreate's registration
	// commit, before login's auth flow (and so its credential write) ever
	// starts.
	real := filepath.Join(dir, "config.real.yaml")
	realCfg := &config.Config{CredentialStore: "exec:fake", Servers: map[string]config.Server{}}
	realCfg.SetPath(real)
	require.NoError(t, realCfg.Write())
	require.NoError(t, os.Symlink(real, config.FilePath(dir)))

	store, err := config.OpenStore("exec:fake", dir)
	require.NoError(t, err)

	cmd := &loginCmd{dir: dir, newStore: func(string, string) (config.CredStore, string, error) { return store, "", nil }}
	_, err = cmd.Execute(t.Context(),
		cmdtest.Input{Str: map[string]string{"url": "https://ghost.ex"}, Flag: map[string]bool{"token-stdin": true}, Pos: []string{"ghost"}},
		cmdtest.StdinPrompter("id:secret"))
	require.Error(t, err, "the registration config write must fail (symlink refusal)")
	assert.Empty(t, cmdtest.HelperCreds(t, statePath), "no credential is ever stored when registration itself fails")
}

// TestLogoutAllNeverRepairsADanglingCurrentServer asserts the retired repair
// behavior: logout --all is a pure credential-store writer now - pointer
// bootstrap/repair is registration's privilege alone, so a dangling pointer
// survives --all exactly as it was.
func TestLogoutAllNeverRepairsADanglingCurrentServer(t *testing.T) {
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

	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	cfgAfter, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Equal(t, "ghost", cfgAfter.CurrentServer, "the dangling pointer is left exactly as it was")
}

// TestDeleteFailureCancellationPassesThrough asserts the fourth-review P1: a
// delete interrupted by the caller's own cancellation must surface as
// context.Canceled through logout's failure shaping - never rewrapped into
// the generic "failed to log out" clierr, which severs the errors.Is chain
// and reads as a helper fault.
func TestDeleteFailureCancellationPassesThrough(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("locking the credential-helper records: %w", context.Canceled)
	err := deleteFailure("failed to log out \"prod\"", wrapped, []string{"a"}, nil, nil, nil)
	require.ErrorIs(t, err, context.Canceled)
	var ce *clierr.Error
	assert.NotErrorAs(t, err, &ce, "cancellation must not be dressed as a clierr delete failure")

	real := deleteFailure("failed to log out \"prod\"", fmt.Errorf("helper exploded"), []string{"a"}, nil, nil, nil)
	require.ErrorAs(t, real, &ce)
	details, ok := ce.Details.(LogoutAllPartialFailure)
	require.True(t, ok)
	assert.Contains(t, details.TextDetails()[0], "already logged out")
}

// TestDeleteFailureCarriesNoticesFromASuccessfulEarlierStep asserts that a
// notice recorded by a step that already succeeded (a keychain-unreachable
// degrade during one server's delete, say) before a LATER step in the same
// --all run fails rides the resulting error instead of being discarded with
// the Result the pipeline never gets to build.
func TestDeleteFailureCarriesNoticesFromASuccessfulEarlierStep(t *testing.T) {
	t.Parallel()
	err := deleteFailure("failed to log out \"prod\"", fmt.Errorf("helper exploded"), []string{"a"}, nil, nil,
		[]string{"keychain unreachable, deleted the file store only"})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, []string{"keychain unreachable, deleted the file store only"}, ce.Warnings())
}

// TestLogoutAllPartialFailureTextDetailsBracketedLines asserts the two
// conditional lines: a deletedFrom entry prints one "credentials removed:"
// line per server, sorted by name; "cached kubectl credentials removed"
// prints only when non-empty.
func TestLogoutAllPartialFailureTextDetailsBracketedLines(t *testing.T) {
	t.Parallel()
	t.Run("no deletedFrom entries and no cache", func(t *testing.T) {
		t.Parallel()
		pf := LogoutAllPartialFailure{LoggedOut: []string{"prod"}}
		assert.Equal(t, []string{
			"already logged out: prod",
			"those credentials are gone regardless of this failure - re-run auth logout --all to finish",
		}, pf.TextDetails())
	})

	t.Run("multiple servers sorted, and non-empty cache", func(t *testing.T) {
		t.Parallel()
		pf := LogoutAllPartialFailure{
			LoggedOut:    []string{"prod", "staging"},
			DeletedFrom:  map[string][]string{"staging": {"keychain"}, "prod": {"keychain", "file"}},
			DeletedCache: []string{"k1", "k2"},
		}
		assert.Equal(t, []string{
			"already logged out: prod, staging",
			"credentials removed: prod: keychain, file",
			"credentials removed: staging: keychain",
			"cached kubectl credentials removed: k1, k2",
			"those credentials are gone regardless of this failure - re-run auth logout --all to finish",
		}, pf.TextDetails())
	})
}

// TestDeleteFailurePreservesLockTimeout asserts the exercise-8/9-fixes review
// finding: a LOCK_TIMEOUT surfacing from the token-cache or helper-records
// lock inside the delete must keep its code and retryability through logout's
// failure shaping - flattening it to GENERIC hides exit 8 and the retry
// signal from scripted callers.
func TestDeleteFailurePreservesLockTimeout(t *testing.T) {
	t.Parallel()
	inner := clierr.Retryable(clierr.CodeLockTimeout,
		"locking token-cache.yaml (another rancher process may be writing): context deadline exceeded")
	err := deleteFailure("failed to log out \"prod\"", inner, []string{"a"}, nil, nil, nil)
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeLockTimeout, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Equal(t, 8, ce.ExitCode())
	assert.Contains(t, ce.Message, "failed to log out")
	details, ok := ce.Details.(LogoutAllPartialFailure)
	require.True(t, ok)
	assert.Contains(t, details.TextDetails()[0], "already logged out")
}

// TestLogoutTextAndCurrentServerClearedReported verifies the TTY story: logout
// is no longer silent - the text names the server, its URL, the backends the
// token was removed from, and that the current server was cleared - and the
// envelope carries currentServerCleared for programs.
// TestLogoutNeverClearsPointerAndOmitsCurrentServerClearedField asserts the
// envelope change: currentServerCleared is REMOVED from LogoutResult -
// logout is a pure credential-store writer and the pointer is untouched
// even for the current server.
func TestLogoutNeverClearsPointerAndOmitsCurrentServerClearedField(t *testing.T) {
	dir, _ := seedLogout(t, "acme", "acme", "other")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{}, ui.Discard())
	require.NoError(t, err)

	data := res.Data.(map[string]any)
	_, hasField := data["currentServerCleared"]
	assert.False(t, hasField, "currentServerCleared must not appear in the envelope at all")

	assert.Contains(t, res.Text, "logged out of acme (https://acme.ex)")
	assert.Contains(t, res.Text, "token removed from: file")
	assert.NotContains(t, res.Text, "current server cleared")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "acme", cfg.CurrentServer, "current server untouched")
}

// TestLogoutTextNoCurrentServerClearWhenOtherServer makes sure logging out a
// server other than the current one says nothing about the current server
// pointer - it was not touched.
func TestLogoutTextNoCurrentServerClearWhenOtherServer(t *testing.T) {
	dir, _ := seedLogout(t, "acme", "acme", "other")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"other"}}, ui.Discard())
	require.NoError(t, err)

	assert.Contains(t, res.Text, "logged out of other (https://other.ex)")
	assert.NotContains(t, res.Text, "current server cleared")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "acme", cfg.CurrentServer, "current server untouched")
}

// TestStyleLogoutTextStripInvariant asserts auth logout's bold spans: each
// server's opener line is bold, whole line; its detail line stays plain;
// the trailing exec-credential cache-count line stays plain too - plus the
// strip invariant and the inactive-style identity.
func TestStyleLogoutTextStripInvariant(t *testing.T) {
	t.Parallel()
	urlByName := map[string]string{"acme": "https://acme.ex", "other": "https://other.ex"}
	deletedFrom := map[string][]string{"acme": {"keychain"}, "other": nil}
	text := renderLogoutText(urlByName, deletedFrom, 3)
	enabled := style.Style{Enabled: true}

	styled := styleLogoutText(text, len(deletedFrom), enabled)
	assert.NotEqual(t, text, styled)
	assert.Equal(t, text, style.Strip(styled))
	assert.Equal(t, text, styleLogoutText(text, len(deletedFrom), style.Style{}), "disabled style returns text unchanged")

	assert.Contains(t, styled, "\x1b[1mlogged out of acme (https://acme.ex)\x1b[0m\n")
	assert.Contains(t, styled, "\x1b[1mlogged out of other (https://other.ex)\x1b[0m\n")
	assert.NotContains(t, styled, "\x1b[1m  token removed from", "a detail line must not be styled")
	assert.NotContains(t, styled, "\x1b[1m  no stored token found", "a detail line must not be styled")
	assert.NotContains(t, styled, "\x1b[1mexec-credential cache entries removed", "the cache-count line must not be styled")
}

// TestLogoutStampsLoggedOutAtOnCLIOwnedEntries asserts logout records the
// time on an entry whose token is CLI-owned, leaves a pre-provenance entry
// untouched, and turns a failed stamp write into a warning.
func TestLogoutStampsLoggedOutAtOnCLIOwnedEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvCredentialStore, "file")
	cfg := &config.Config{CurrentServer: "prod", Servers: map[string]config.Server{
		"prod":  {URL: "https://r.ex", Provenance: clierr.ProvenanceDerived, TokenRef: &clierr.TokenRef{Kind: clierr.TokenKindExt, Name: "t"}},
		"plain": {URL: "https://p.ex"},
	}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Flag: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	cfg, _ = config.Load(config.FilePath(dir))
	assert.NotEmpty(t, cfg.Servers["prod"].LoggedOutAt)
	_, perr := time.Parse(time.RFC3339, cfg.Servers["prod"].LoggedOutAt)
	require.NoError(t, perr)
	assert.Empty(t, cfg.Servers["plain"].LoggedOutAt)
	assert.Equal(t, "prod", cfg.CurrentServer, "logout still never clears the pointer")

	restore := config.SetPersistWriteHookForTest(func(step string) error {
		if step == "logout" {
			return errors.New("read-only")
		}
		return nil
	})
	t.Cleanup(restore)
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), cmdtest.Input{Pos: []string{"prod"}}, ui.Discard())
	require.NoError(t, err)
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "could not record the logout")
}
