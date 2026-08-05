package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// seedDeleteServer mirrors logout_test.go's seedLogout for config/file setup
// ONLY: a real FileStore + an in-memory keychain (keyring.MockInit), no fake
// store. contexts maps a context KEY to the server name it targets, so a
// test can alias a context under a key other than the server name.
func seedDeleteServer(t *testing.T, current string, servers []string, contexts map[string]string) (string, config.CredStore) {
	t.Helper()
	keyring.MockInit()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	cfg := &config.Config{
		CurrentContext: current,
		Servers:        map[string]config.Server{},
		Contexts:       map[string]config.Context{},
	}
	for _, s := range servers {
		cfg.Servers[s] = config.Server{URL: "https://" + s + ".ex"}
		require.NoError(t, store.SetToken(config.ServerRef{Name: s}, "id:sec"))
	}
	for ctxName, server := range contexts {
		cfg.Contexts[ctxName] = config.Context{Server: server}
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir, store
}

func TestConfigDeleteServerDeleteFailureAbortsBeforeAnyWrite(t *testing.T) {
	dir, _ := seedDeleteServer(t, "acme", []string{"acme", "other"}, map[string]string{"acme": "acme", "other": "other"})
	path := config.FilePath(dir)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	cmd := NewConfigDeleteServer(dir).(*configDeleteServerCmd)
	deleteErr := errors.New("keychain locked")
	cmd.delete = func(_ context.Context, dir string, ref config.ServerRef, policy string) ([]string, []string, error) {
		return nil, nil, deleteErr
	}

	_, err = cmd.Execute(context.Background(), args{a: []string{"acme"}}, ui.Discard())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keychain locked")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "config file must be byte-identical when delete fails — the write must never be reached")

	cfg, err := config.Load(path)
	require.NoError(t, err)
	_, ok := cfg.Servers["acme"]
	assert.True(t, ok, "server entry must still be present after a failed delete (benign failure direction: creds gone, server still configured — never the reverse)")
	assert.Equal(t, "acme", cfg.CurrentContext, "current context must be untouched too")
}

func TestConfigDeleteServerRemovesFieldMatchedContextsIncludingAliasedKeys(t *testing.T) {
	dir, store := seedDeleteServer(t, "other", []string{"acme", "other"}, map[string]string{
		"acme":        "acme", // key equals server name
		"weird-alias": "acme", // aliased key: context key != server name, matched by field
		"other":       "other",
	})

	res, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(context.Background(), args{a: []string{"acme"}}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "DeletedServer", res.Kind)

	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "acme", data["server"])
	assert.Equal(t, "https://acme.ex", data["url"])
	assert.ElementsMatch(t, []string{"acme", "weird-alias"}, data["contexts"], "both contexts field-matching server acme are removed, key-equal and aliased alike")
	assert.False(t, data["currentContextCleared"].(bool), "current context (\"other\") targets a different server and is untouched")
	assert.Equal(t, map[string][]string{"backend": {"file"}}, data["deletedFrom"], "mock keychain is empty, so only the file backend reports a delete")

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok = cfg.Servers["acme"]
	assert.False(t, ok, "server entry removed")
	_, ok = cfg.Servers["other"]
	assert.True(t, ok, "unrelated server kept")
	_, ok = cfg.Contexts["acme"]
	assert.False(t, ok)
	_, ok = cfg.Contexts["weird-alias"]
	assert.False(t, ok, "aliased context removed by field match, not by key")
	_, ok = cfg.Contexts["other"]
	assert.True(t, ok, "unrelated context kept")
	assert.Equal(t, "other", cfg.CurrentContext)

	_, err = store.GetToken(config.ServerRef{Name: "acme"})
	assert.ErrorIs(t, err, config.ErrNoToken, "token deleted")
	_, err = store.GetToken(config.ServerRef{Name: "other"})
	require.NoError(t, err, "unrelated server's token kept")
}

func TestConfigDeleteServerClearsCurrentContextWhenAliasedKeyWasCurrent(t *testing.T) {
	dir, _ := seedDeleteServer(t, "weird-alias", []string{"acme"}, map[string]string{
		"weird-alias": "acme",
	})

	res, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(context.Background(), args{a: []string{"acme"}}, ui.Discard())
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	assert.True(t, data["currentContextCleared"].(bool))

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "", cfg.CurrentContext, "current context cleared: it aliased the removed server by field, not by a matching key")
}

func TestConfigDeleteServerUnknownNameIsNotFoundWithCandidates(t *testing.T) {
	dir, _ := seedDeleteServer(t, "", []string{"acme"}, nil)

	_, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(context.Background(), args{a: []string{"nope"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
	ids := make([]string, 0, len(ce.Candidates))
	for _, c := range ce.Candidates {
		ids = append(ids, c.ID)
	}
	assert.Contains(t, ids, "acme")
}

func TestConfigDeleteServerDeletesExecCredentialCache(t *testing.T) {
	dir, store := seedDeleteServer(t, "", []string{"acme"}, map[string]string{"acme": "acme"})
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), "tok", time.Time{}, "kubeuser", "acme.ex", "c-1"))

	res, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(context.Background(), args{a: []string{"acme"}}, ui.Discard())
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	assert.Equal(t, []string{config.CacheKey("kubeuser", "acme.ex", "c-1")}, data["deletedCache"])

	_, _, err = store.GetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"))
	assert.ErrorIs(t, err, config.ErrNoToken)
}

// TestConfigDeleteServerWritesNoCredstorePin is SUPPLEMENTARY coverage of
// the delete's own store-independence (spec R2-1): it bypasses resolveDeps
// by construction, so it passes before AND after the adapter swap — the
// real resolveDeps regression lives in surface/cli's through-the-root
// tests (Task 3 Step 0). This one pins that the command itself never
// resolves the injected store.
func TestConfigDeleteServerWritesNoCredstorePin(t *testing.T) {
	t.Setenv(config.EnvCredentialStore, "")
	dir, _ := seedDeleteServer(t, "acme", []string{"acme"}, map[string]string{"acme": "acme"})

	_, err := NewConfigDeleteServer(dir).(*configDeleteServerCmd).Execute(
		context.Background(), args{a: []string{"acme"}}, ui.Discard())
	require.NoError(t, err)

	_, statErr := os.Stat(config.PinPath(dir))
	assert.True(t, os.IsNotExist(statErr), "delete must not write a credstore pin")
}

func TestConfigDeleteServerErasesThroughHelper(t *testing.T) {
	dir, statePath := seedExecLogout(t, "acme", "acme")
	_, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{a: []string{"acme"}}, ui.Discard())
	require.NoError(t, err)
	assert.Empty(t, helperCreds(t, statePath), "helper-held credential erased before config removal")
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["acme"]
	assert.False(t, ok, "server entry removed after a successful delete")
}

func TestConfigDeleteServerFailedEraseBlocksRemoval(t *testing.T) {
	dir, statePath := seedExecLogout(t, "acme", "acme")
	t.Setenv("FAKEHELPER_MODE", "fail") // helper now refuses to erase
	_, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{a: []string{"acme"}}, ui.Discard())
	require.Error(t, err)
	assert.NotEmpty(t, helperCreds(t, statePath), "credential still held by the helper")
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	_, ok := cfg.Servers["acme"]
	assert.True(t, ok, "failed delete leaves the server config untouched")
}

// TestConfigDeleteServerCompleteArgsDelegatesToServerNames proves only the
// wiring: sort order is a ConfigView-level fact, pinned once at
// newConfigView in surface/cli.
func TestConfigDeleteServerCompleteArgsDelegatesToServerNames(t *testing.T) {
	t.Parallel()
	view := testView{srvNames: []string{"acme"}}
	got := configDeleteServerCmd{}.CompleteArgs(context.Background(), view)
	assert.Equal(t, []string{"acme"}, got)
}

// TestDeleteServerCancellationPassesThrough pins the fourth-review P1 for
// config delete-server: a cancellation surfacing from the delete must pass
// through unchanged, not be rewrapped as a generic delete failure.
func TestDeleteServerCancellationPassesThrough(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{"acme": {URL: "https://acme.ex"}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	cmd := &configDeleteServerCmd{dir: dir, delete: func(context.Context, string, config.ServerRef, string) ([]string, []string, error) {
		return nil, nil, fmt.Errorf("records lock: %w", context.Canceled)
	}}
	_, err := cmd.Execute(context.Background(), args{a: []string{"acme"}}, ui.Discard())
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), "server config left untouched")

	cfgAfter, lerr := config.Load(config.FilePath(dir))
	require.NoError(t, lerr)
	_, ok := cfgAfter.Servers["acme"]
	assert.True(t, ok, "the server entry must be untouched after a cancelled delete")
}

// TestConfigDeleteServerDeleteLockTimeoutSurvives pins the exercise-8/9-fixes
// review finding for this command: a classified LOCK_TIMEOUT from the delete
// keeps its code, retryability, and exit status through the
// "server config left untouched" wrap.
func TestConfigDeleteServerDeleteLockTimeoutSurvives(t *testing.T) {
	dir, _ := seedDeleteServer(t, "acme", []string{"acme"}, map[string]string{"acme": "acme"})

	cmd := NewConfigDeleteServer(dir).(*configDeleteServerCmd)
	cmd.delete = func(context.Context, string, config.ServerRef, string) ([]string, []string, error) {
		return nil, nil, clierr.Retryable(clierr.CodeLockTimeout,
			"locking the credential-helper records (another rancher process may be logging in/out): context deadline exceeded")
	}

	_, err := cmd.Execute(context.Background(), args{a: []string{"acme"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeLockTimeout, ce.Code)
	assert.True(t, ce.Retryable)
	assert.Equal(t, 8, ce.ExitCode())
	assert.Contains(t, ce.Message, "server config left untouched")
}
