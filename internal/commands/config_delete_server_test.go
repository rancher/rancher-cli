package commands

import (
	"context"
	"errors"
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
		require.NoError(t, store.SetToken(s, "id:sec"))
	}
	for ctxName, server := range contexts {
		cfg.Contexts[ctxName] = config.Context{Server: server}
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	return dir, store
}

func TestConfigDeleteServerDrainFailureAbortsBeforeAnyWrite(t *testing.T) {
	dir, _ := seedDeleteServer(t, "acme", []string{"acme", "other"}, map[string]string{"acme": "acme", "other": "other"})
	path := config.FilePath(dir)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	cmd := NewConfigDeleteServer(dir).(*configDeleteServerCmd)
	drainErr := errors.New("keychain locked")
	cmd.drain = func(dir, server, url string) ([]string, []string, error) {
		return nil, nil, drainErr
	}

	_, err = cmd.Execute(context.Background(), args{a: []string{"acme"}}, ui.Discard())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keychain locked")

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "config file must be byte-identical when drain fails — the write must never be reached")

	cfg, err := config.Load(path)
	require.NoError(t, err)
	_, ok := cfg.Servers["acme"]
	assert.True(t, ok, "server entry must still be present after a failed drain (benign failure direction: creds gone, server still configured — never the reverse)")
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
	assert.Equal(t, map[string][]string{"backend": {"file"}}, data["deletedFrom"], "mock keychain is empty, so only the file backend reports a drain")

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

	_, err = store.GetToken("acme")
	assert.ErrorIs(t, err, config.ErrNoToken, "token drained")
	_, err = store.GetToken("other")
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

func TestConfigDeleteServerDrainsExecCredentialCache(t *testing.T) {
	dir, store := seedDeleteServer(t, "", []string{"acme"}, map[string]string{"acme": "acme"})
	require.NoError(t, store.SetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"), "tok", time.Time{}, "kubeuser", "acme.ex", "c-1"))

	res, err := NewConfigDeleteServer(dir).(runtime.ReadCommand).Execute(context.Background(), args{a: []string{"acme"}}, ui.Discard())
	require.NoError(t, err)
	data := res.Data.(map[string]any)
	assert.Equal(t, []string{config.CacheKey("kubeuser", "acme.ex", "c-1")}, data["deletedCache"])

	_, _, err = store.GetCachedCred(config.CacheKey("kubeuser", "acme.ex", "c-1"))
	assert.ErrorIs(t, err, config.ErrNoToken)
}

// TestConfigDeleteServerCompleteArgsDelegatesToServerNames proves only the
// wiring: sort order and the missing/corrupt-config degrade-to-empty
// behavior are serverNames-level facts, pinned once in names_test.go
// (external review findings 8+9).
func TestConfigDeleteServerCompleteArgsDelegatesToServerNames(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{"acme": {}}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	assert.Equal(t, serverNames(dir), configDeleteServerCmd{}.CompleteArgs(context.Background(), dir))
}
