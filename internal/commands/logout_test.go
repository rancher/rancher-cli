package commands

import (
	"context"
	"errors"
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

// seedConfig writes a config with the given current context and servers/contexts,
// and stores a token per server. Returns dir + store.
func seedLogout(t *testing.T, current string, servers ...string) (string, config.CredStore) {
	t.Helper()
	keyring.MockInit() // in-memory keychain: DrainToken now drains both backends unconditionally
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{
		CurrentContext: current,
		Servers:        map[string]config.Server{},
		Contexts:       map[string]config.Context{},
	}
	for _, s := range servers {
		cfg.Servers[s] = config.Server{URL: "https://" + s + ".ex"}
		cfg.Contexts[s] = config.Context{Server: s}
		require.NoError(t, store.SetToken(config.ServerRef{Name: s}, "id:sec"))
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir, store
}

func TestLogoutNoArgLogsOutCurrent(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme", "other")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "LogoutResult", res.Kind)

	data, isMap := res.Data.(map[string]any)
	require.True(t, isMap, "Result data is a map")
	df, isDF := data["deletedFrom"].(map[string][]string)
	require.True(t, isDF, "Result carries per-server deletedFrom")
	assert.Equal(t, []string{"file"}, df["acme"], "mock keychain is empty, so only the file backend reports a drain")

	_, err = store.GetToken(config.ServerRef{Name: "acme"})
	assert.ErrorIs(t, err, config.ErrNoToken)
	_, err = store.GetToken(config.ServerRef{Name: "other"})
	require.NoError(t, err) // untouched

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "", cfg.CurrentContext) // unset
	_, ok := cfg.Servers["acme"]
	assert.True(t, ok, "server config entry kept") // token-only removal
}

func TestLogoutByName(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme", "other")
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(), args{a: []string{"other"}}, ui.Discard())
	require.NoError(t, err)
	_, err = store.GetToken(config.ServerRef{Name: "other"})
	assert.ErrorIs(t, err, config.ErrNoToken)
	_, err = store.GetToken(config.ServerRef{Name: "acme"})
	require.NoError(t, err) // current untouched
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "acme", cfg.CurrentContext) // not unset (other != current)
}

func TestLogoutUnknownNameIsNotFoundWithCandidates(t *testing.T) {
	dir, _ := seedLogout(t, "acme", "acme")
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(), args{a: []string{"nope"}}, ui.Discard())
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
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(), args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	_, err = store.GetToken(config.ServerRef{Name: "acme"})
	assert.ErrorIs(t, err, config.ErrNoToken)
	_, err = store.GetToken(config.ServerRef{Name: "other"})
	assert.ErrorIs(t, err, config.ErrNoToken)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "", cfg.CurrentContext)
}

func TestLogoutDrainsMatchingExecCache(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme", "other")
	// acme.ex and other.ex per seedLogout; cache two entries, one per host,
	// plus a legacy bare entry (no Server) that must never be touched.
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), "tok-acme", time.Time{}, "kubeuser", "acme.ex", "c-1"))
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "other.ex", "c-1"), "tok-other", time.Time{}, "kubeuser", "other.ex", "c-1"))
	require.NoError(t, store.SetCachedCred("legacy-bare", "tok-legacy", time.Time{}, "", "", ""))

	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(), args{a: []string{"acme"}}, ui.Discard())
	require.NoError(t, err)

	data := res.Data.(map[string]any)
	deletedCache, ok := data["deletedCache"].([]string)
	require.True(t, ok, "Result carries deletedCache")
	assert.Equal(t, []string{config.CacheKey("kubeuser", "acme.ex", "c-1")}, deletedCache)

	_, _, err = store.GetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"))
	assert.ErrorIs(t, err, config.ErrNoToken, "matching entry drained")
	_, _, err = store.GetCachedCred(config.CacheKey("kubeuser", "other.ex", "c-1"))
	require.NoError(t, err, "non-matching entry kept")
	_, _, err = store.GetCachedCred("legacy-bare")
	require.NoError(t, err, "legacy bare entry (no Server) kept")
}

func TestLogoutAllDrainsWholeExecCache(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme", "other")
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), "tok-acme", time.Time{}, "kubeuser", "acme.ex", "c-1"))
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "other.ex", "c-1"), "tok-other", time.Time{}, "kubeuser", "other.ex", "c-1"))

	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(), args{b: map[string]bool{"all": true}}, ui.Discard())
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

// Round-3 repro: --all's per-server host-scoped drain (round 2) missed a
// legacy bare entry (no Server field) and an entry cached against a server
// no longer in config.yaml — neither is reachable by any per-server drain.
// --all must catch both via the full-cache wipe.
func TestLogoutAllCatchesOrphanedCacheEntries(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme")
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), "tok-acme", time.Time{}, "kubeuser", "acme.ex", "c-1"))
	// Orphaned: no Server field at all (legacy bare entry).
	require.NoError(t, store.SetCachedCred("legacy-bare", "tok-legacy", time.Time{}, "", "", ""))
	// Orphaned: cached against a host with no matching entry in cfg.Servers.
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "removed.ex", "c-1"), "tok-removed", time.Time{}, "kubeuser", "removed.ex", "c-1"))

	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(), args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)

	data := res.Data.(map[string]any)
	deletedCache, ok := data["deletedCache"].([]string)
	require.True(t, ok)
	assert.ElementsMatch(t, []string{
		config.CacheKey("kubeuser", "acme.ex", "c-1"),
		"legacy-bare",
		config.CacheKey("kubeuser", "removed.ex", "c-1"),
	}, deletedCache, "--all must catch orphaned entries no per-server drain can reach")

	creds, err := store.ListCachedCreds()
	require.NoError(t, err)
	assert.Empty(t, creds, "no orphaned live tokens after --all")
}

func TestLogoutNameAndAllIsValidation(t *testing.T) {
	dir, _ := seedLogout(t, "acme", "acme")
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{a: []string{"acme"}, b: map[string]bool{"all": true}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLogoutNoCurrentContext(t *testing.T) {
	dir, _ := seedLogout(t, "", "acme") // no current context
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeContextNotSet, ce.Code)
}

func TestLogoutCurrentContextKeyAbsent(t *testing.T) {
	dir, _ := seedLogout(t, "ghost", "acme") // CurrentContext points at a missing key
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeContextNotSet, ce.Code)
}

func TestLogoutCurrentContextServerAbsent(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentContext: "acme",
		Servers:        map[string]config.Server{}, // server missing
		Contexts:       map[string]config.Context{"acme": {Server: "gone"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeContextNotSet, ce.Code)
}

func TestLogoutExecPolicyReportsHelper(t *testing.T) {
	dir, statePath := seedExecLogout(t, "acme", "acme")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	require.NoError(t, err)
	df := res.Data.(map[string]any)["deletedFrom"].(map[string][]string)
	assert.Contains(t, df["acme"], "helper:fake")
	assert.Empty(t, helperCreds(t, statePath), "helper-held credential erased")
}

func TestLogoutAllExecPolicyErasesEveryServer(t *testing.T) {
	dir, statePath := seedExecLogout(t, "acme", "acme", "other")
	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	df := res.Data.(map[string]any)["deletedFrom"].(map[string][]string)
	assert.Contains(t, df["acme"], "helper:fake")
	assert.Contains(t, df["other"], "helper:fake")
	assert.Empty(t, helperCreds(t, statePath), "--all erases every helper-held credential")
}

// TestLogoutAllDrainsOrphanedHelperCredential is the impl-review
// finding-2 repro: a helper credential recorded for a server
// name absent from config.yaml (left behind by a crash/failed write) is unreachable
// from the per-server logout loop, so `logout --all` must drain it via the
// orphaned-record path and report it.
func TestLogoutAllDrainsOrphanedHelperCredential(t *testing.T) {
	t.Setenv("PATH", buildCommandsFakeHelper(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKEHELPER_MODE", "ok")
	statePath := filepath.Join(t.TempDir(), "state.json")
	t.Setenv("FAKEHELPER_STATE", statePath)
	keyring.MockInit()

	dir := t.TempDir()
	cfg := &config.Config{
		CredentialStore: "exec:fake",
		Servers:         map[string]config.Server{},
		Contexts:        map[string]config.Context{},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	// Store a helper credential + its record for a name that never made it
	// into config.yaml.
	store, err := config.OpenStore("exec:fake", dir)
	require.NoError(t, err)
	require.NoError(t, store.SetToken(config.ServerRef{Name: "ghost", URL: "https://ghost.ex"}, "id:sec"))
	require.NotEmpty(t, helperCreds(t, statePath), "precondition: helper holds the orphaned credential")

	res, err := NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)

	df := res.Data.(map[string]any)["deletedFrom"].(map[string][]string)
	assert.Contains(t, df["ghost"], "helper:fake", "logout --all must report the drained orphan under its server name")
	assert.Empty(t, helperCreds(t, statePath), "the orphaned helper credential must be erased")

	residue, err := config.DrainOrphanedHelperCredentials(context.Background(), dir)
	require.NoError(t, err)
	assert.Empty(t, residue, "provenance must be fully drained")
}

// TestLogoutAllDrainsResidueAfterFailedConfigWrite is finding-2's review-
// requested scenario: a login whose helper store succeeds but whose config
// write then fails strands a live helper credential + helper record for a
// server never persisted to config.yaml. `logout --all` must still clean it.
func TestLogoutAllDrainsResidueAfterFailedConfigWrite(t *testing.T) {
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
	realCfg := &config.Config{CredentialStore: "exec:fake", Servers: map[string]config.Server{}, Contexts: map[string]config.Context{}}
	realCfg.SetPath(real)
	require.NoError(t, realCfg.Write())
	require.NoError(t, os.Symlink(real, config.FilePath(dir)))

	store, err := config.OpenStore("exec:fake", dir)
	require.NoError(t, err)

	_, err = newLoginCmd(dir, store).Execute(context.Background(),
		args{s: map[string]string{"server": "https://ghost.ex", "name": "ghost"}, b: map[string]bool{"token-stdin": true}},
		stdinPrompter("id:secret"))
	require.Error(t, err, "the config write must fail (symlink refusal)")
	require.NotEmpty(t, helperCreds(t, statePath), "the helper stored the credential before the config write failed")

	// logout --all reads the unchanged config (ghost is absent from it) yet
	// must still drain the stranded helper credential via provenance.
	_, err = NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	assert.Empty(t, helperCreds(t, statePath), "logout --all must clean the stranded residue")

	residue, err := config.DrainOrphanedHelperCredentials(context.Background(), dir)
	require.NoError(t, err)
	assert.Empty(t, residue)
}

func TestLogoutAllClearsDanglingCurrentContext(t *testing.T) {
	keyring.MockInit()
	// CurrentContext points at a context key that doesn't exist.
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{
		CurrentContext: "ghost",
		Servers:        map[string]config.Server{"acme": {URL: "https://acme.ex"}},
		Contexts:       map[string]config.Context{"acme": {Server: "acme"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	require.NoError(t, store.SetToken(config.ServerRef{Name: "acme"}, "id:sec"))

	_, err := NewLogout(dir).(runtime.ReadCommand).Execute(context.Background(), args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	cfgAfter, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Equal(t, "", cfgAfter.CurrentContext)
}

// TestDrainFailureCancellationPassesThrough pins the fourth-review P1: a
// drain interrupted by the caller's own cancellation must surface as
// context.Canceled through logout's failure shaping — never rewrapped into
// the generic "failed to log out" clierr, which severs the errors.Is chain
// and reads as a helper fault.
func TestDrainFailureCancellationPassesThrough(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("locking the credential-helper records: %w", context.Canceled)
	err := drainFailure("failed to log out \"prod\"", wrapped, []string{"a"})
	assert.ErrorIs(t, err, context.Canceled)
	var ce *clierr.Error
	assert.False(t, errors.As(err, &ce), "cancellation must not be dressed as a clierr drain failure")

	real := drainFailure("failed to log out \"prod\"", fmt.Errorf("helper exploded"), []string{"a"})
	require.ErrorAs(t, real, &ce)
	assert.Contains(t, ce.Hint, "already logged out")
}
