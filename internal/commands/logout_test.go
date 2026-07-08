package commands

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedConfig writes a config with the given current context and servers/contexts,
// and stores a token per server. Returns dir + store.
func seedLogout(t *testing.T, current string, servers ...string) (string, config.CredStore) {
	t.Helper()
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
	t.Parallel()
	dir, store := seedLogout(t, "acme", "acme", "other")
	res, err := NewLogout(dir, store).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "LogoutResult", res.Kind)

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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestLogoutNameAndAllIsValidation(t *testing.T) {
	t.Parallel()
	dir, store := seedLogout(t, "acme", "acme")
	_, err := NewLogout(dir, store).(runtime.ReadCommand).Execute(context.Background(),
		args{a: []string{"acme"}, b: map[string]bool{"all": true}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestLogoutNoCurrentContext(t *testing.T) {
	t.Parallel()
	dir, store := seedLogout(t, "", "acme") // no current context
	_, err := NewLogout(dir, store).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeContextNotSet, ce.Code)
}

func TestLogoutCurrentContextKeyAbsent(t *testing.T) {
	t.Parallel()
	dir, store := seedLogout(t, "ghost", "acme") // CurrentContext points at a missing key
	_, err := NewLogout(dir, store).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeContextNotSet, ce.Code)
}

func TestLogoutCurrentContextServerAbsent(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
