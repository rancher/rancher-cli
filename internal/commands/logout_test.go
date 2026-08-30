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
	"github.com/rancher/rancher-cli/internal/output/style"
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
	assert.Equal(t, "acme", cfg.CurrentServer, "logout never clears the pointer, even for the current server")
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
	assert.Equal(t, "acme", cfg.CurrentServer, "logout --all no longer resets the pointer — a credential-less entry is a valid state")
}

// TestLogoutAllOnEmptyConfigReturnsText pins that logging out of every
// server when none are configured is still a reportable success: ShapeText
// requires non-empty Text on every default-format success, and a config
// with zero servers is the one path where deletedFrom's per-server loop
// produces nothing to report.
func TestLogoutAllOnEmptyConfigReturnsText(t *testing.T) {
	dir, _ := seedLogout(t, "")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	assert.NotEmpty(t, res.Text, "logout --all on an empty config must still produce operator-facing text")
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
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), "tok-acme", time.Time{}, "kubeuser", "acme.ex", "c-1"))
	require.NoError(t, os.Chmod(filepath.Join(dir, "token-cache.yaml"), 0o644))

	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	require.Len(t, res.Warnings, 1, "the exec-cache store's loose-permission notice must reach Result.Warnings")
	assert.Contains(t, res.Warnings[0], "group/world-readable")
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

// TestLogoutAllOrphanSweepFailureReportsWhatAlreadySucceeded pins that a
// failure in the LAST of deleteAll's three steps must not discard the
// first two: the per-server loop and exec-cache wipe both succeed before
// the orphan sweep runs, so their results must survive a sweep failure,
// not be vaporized. "acme" is a plain file-store entry (its deletion never touches
// the helper-records file at all, exec policy or not); "ghost" is an
// orphaned EXEC-helper record, opened directly against the "exec:fake"
// backend independent of cfg's own "file" policy — matching how a real
// orphan arises (left behind by a PAST exec policy, swept regardless of
// the CURRENT one). Only the orphan sweep's persist write is refused.
func TestLogoutAllOrphanSweepFailureReportsWhatAlreadySucceeded(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("symlink-based write failure needs POSIX symlink semantics")
	}
	t.Setenv("PATH", buildCommandsFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
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

	// A helper credential for a name never written to config.yaml — the
	// orphan the sweep step exists to reach — opened directly against
	// exec:fake regardless of cfg's "file" policy above.
	execStore, err := config.OpenStore("exec:fake", dir)
	require.NoError(t, err)
	require.NoError(t, execStore.SetToken(config.ServerRef{Name: "ghost", URL: "https://ghost.ex"}, "id:sec"))

	// credhelper-provenance.yaml (internal/config's helperRecordsFileName)
	// behind a symlink: reads during the sweep still resolve, but its
	// persist write is refused — the per-server loop (file-backed "acme")
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
		args{b: map[string]bool{"all": true}}, ui.Discard())
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
	assert.Equal(t, "those credentials are gone regardless of this failure — re-run auth logout --all to finish",
		lines[len(lines)-1])

	_, gerr := fileStore.GetToken(config.ServerRef{Name: "acme", URL: "https://acme.ex"})
	assert.ErrorIs(t, gerr, config.ErrNoToken, "acme's credential deletion is not undone by the later failure")
}

// TestDelegatedLoginConfigWriteFailureNeverStrandsHelperCredential pins a
// safety property the server-registration restructure adds: registration
// (config.yaml) commits BEFORE any authentication or credential write, in
// its own lock cycle. A registration-time config-write failure must
// therefore leave no helper credential behind at all — the interleaving
// (helper store succeeds, then config write fails) that used to strand a
// credential is now structurally impossible, because delegated login can
// no longer reach the credential-write step without registration's own
// cfg.Write() having already succeeded. TestLogoutAllDeletesOrphanedHelperCredential
// covers `logout --all`'s orphan sweep directly, independent of login.
func TestDelegatedLoginConfigWriteFailureNeverStrandsHelperCredential(t *testing.T) {
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
		args{s: map[string]string{"url": "https://ghost.ex"}, b: map[string]bool{"token-stdin": true}, a: []string{"ghost"}},
		stdinPrompter("id:secret"))
	require.Error(t, err, "the registration config write must fail (symlink refusal)")
	assert.Empty(t, helperCreds(t, statePath), "no credential is ever stored when registration itself fails")
}

// TestLogoutAllNeverRepairsADanglingCurrentServer pins the retired repair
// behavior: logout --all is a pure credential-store writer now — pointer
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

	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	cfgAfter, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Equal(t, "ghost", cfgAfter.CurrentServer, "the dangling pointer is left exactly as it was")
}

// TestDeleteFailureCancellationPassesThrough pins the fourth-review P1: a
// delete interrupted by the caller's own cancellation must surface as
// context.Canceled through logout's failure shaping — never rewrapped into
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

// TestDeleteFailureCarriesNoticesFromASuccessfulEarlierStep pins that a
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

// TestLogoutAllPartialFailureTextDetailsBracketedLines pins the two
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
			"those credentials are gone regardless of this failure — re-run auth logout --all to finish",
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
			"those credentials are gone regardless of this failure — re-run auth logout --all to finish",
		}, pf.TextDetails())
	})
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
// is no longer silent — the text names the server, its URL, the backends the
// token was removed from, and that the current server was cleared — and the
// envelope carries currentServerCleared for programs.
// TestLogoutNeverClearsPointerAndOmitsCurrentServerClearedField pins the
// envelope change: currentServerCleared is REMOVED from LogoutResult —
// logout is a pure credential-store writer and the pointer is untouched
// even for the current server.
func TestLogoutNeverClearsPointerAndOmitsCurrentServerClearedField(t *testing.T) {
	dir, _ := seedLogout(t, "acme", "acme", "other")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{}, ui.Discard())
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
// pointer — it was not touched.
func TestLogoutTextNoCurrentServerClearWhenOtherServer(t *testing.T) {
	dir, _ := seedLogout(t, "acme", "acme", "other")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(t.Context(), args{a: []string{"other"}}, ui.Discard())
	require.NoError(t, err)

	assert.Contains(t, res.Text, "logged out of other (https://other.ex)")
	assert.NotContains(t, res.Text, "current server cleared")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "acme", cfg.CurrentServer, "current server untouched")
}

// TestStyleLogoutTextStripInvariant pins auth logout's bold spans: each
// server's opener line is bold, whole line; its detail line stays plain;
// the trailing exec-credential cache-count line stays plain too — plus the
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
