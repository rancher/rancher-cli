package commands

import (
	"context"
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
		require.NoError(t, store.SetToken(s, "id:sec"))
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir, store
}

func TestLogoutNoArgLogsOutCurrent(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme", "other")
	res, err := NewLogout(dir, store).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "LogoutResult", res.Kind)

	data, isMap := res.Data.(map[string]any)
	require.True(t, isMap, "Result data is a map")
	df, isDF := data["deletedFrom"].(map[string][]string)
	require.True(t, isDF, "Result carries per-server deletedFrom")
	assert.Equal(t, []string{"file"}, df["acme"], "mock keychain is empty, so only the file backend reports a drain")

	_, err = store.GetToken("acme")
	assert.ErrorIs(t, err, config.ErrNoToken)
	_, err = store.GetToken("other")
	require.NoError(t, err) // untouched

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "", cfg.CurrentContext) // unset
	_, ok := cfg.Servers["acme"]
	assert.True(t, ok, "server config entry kept") // token-only removal
}

func TestLogoutByName(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme", "other")
	_, err := NewLogout(dir, store).(runtime.ReadCommand).Execute(context.Background(), args{a: []string{"other"}}, ui.Discard())
	require.NoError(t, err)
	_, err = store.GetToken("other")
	assert.ErrorIs(t, err, config.ErrNoToken)
	_, err = store.GetToken("acme")
	require.NoError(t, err) // current untouched
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "acme", cfg.CurrentContext) // not unset (other != current)
}

func TestLogoutUnknownNameIsNotFoundWithCandidates(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme")
	_, err := NewLogout(dir, store).(runtime.ReadCommand).Execute(context.Background(), args{a: []string{"nope"}}, ui.Discard())
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
	_, err := NewLogout(dir, store).(runtime.ReadCommand).Execute(context.Background(), args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	_, err = store.GetToken("acme")
	assert.ErrorIs(t, err, config.ErrNoToken)
	_, err = store.GetToken("other")
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

	res, err := NewLogout(dir, store).(runtime.ReadCommand).Execute(context.Background(), args{a: []string{"acme"}}, ui.Discard())
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

	res, err := NewLogout(dir, store).(runtime.ReadCommand).Execute(context.Background(), args{b: map[string]bool{"all": true}}, ui.Discard())
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

func TestLogoutNameAndAllIsValidation(t *testing.T) {
	dir, store := seedLogout(t, "acme", "acme")
	_, err := NewLogout(dir, store).(runtime.ReadCommand).Execute(context.Background(),
		args{a: []string{"acme"}, b: map[string]bool{"all": true}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLogoutNoCurrentContext(t *testing.T) {
	dir, store := seedLogout(t, "", "acme") // no current context
	_, err := NewLogout(dir, store).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeContextNotSet, ce.Code)
}

func TestLogoutCurrentContextKeyAbsent(t *testing.T) {
	dir, store := seedLogout(t, "ghost", "acme") // CurrentContext points at a missing key
	_, err := NewLogout(dir, store).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeContextNotSet, ce.Code)
}

func TestLogoutCurrentContextServerAbsent(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{
		CurrentContext: "acme",
		Servers:        map[string]config.Server{}, // server missing
		Contexts:       map[string]config.Context{"acme": {Server: "gone"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	_, err := NewLogout(dir, store).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeContextNotSet, ce.Code)
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
	require.NoError(t, store.SetToken("acme", "id:sec"))

	_, err := NewLogout(dir, store).(runtime.ReadCommand).Execute(context.Background(), args{b: map[string]bool{"all": true}}, ui.Discard())
	require.NoError(t, err)
	cfgAfter, cerr := config.Load(config.FilePath(dir))
	require.NoError(t, cerr)
	assert.Equal(t, "", cfgAfter.CurrentContext)
}
