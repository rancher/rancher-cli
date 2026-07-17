package commands

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeServersConfig(t *testing.T, dir string) {
	t.Helper()
	cfg := &config.Config{
		CurrentContext: "acme",
		Contexts:       map[string]config.Context{"acme": {Server: "acme"}},
		Servers:        map[string]config.Server{"acme": {URL: "https://a"}, "beta": {URL: "https://b"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
}

// The ServerList payload must serialize to a non-empty JSON array (regression:
// an unexported field made -o json emit {} while -o table worked).
func TestGetServersJSONCarriesData(t *testing.T) {
	dir := t.TempDir()
	writeServersConfig(t, dir)

	res, err := NewServerLs(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "ServerList", res.Kind)

	b, err := json.Marshal(res.Data)
	require.NoError(t, err)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(b, &got), "ServerList data must be a JSON array")
	require.Len(t, got, 2) // sorted: acme, beta
	assert.Equal(t, "acme", got[0]["name"])
	assert.Equal(t, "https://a", got[0]["url"])
	assert.Equal(t, true, got[0]["current"])
	assert.Equal(t, "beta", got[1]["name"])
	assert.Equal(t, false, got[1]["current"])
}

// -o table / -o name still work: the payload implements output.Tabular.
func TestGetServersTabular(t *testing.T) {
	dir := t.TempDir()
	writeServersConfig(t, dir)

	res, err := NewServerLs(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	require.NoError(t, err)
	tab, ok := res.Data.(output.Tabular)
	require.True(t, ok)
	assert.Equal(t, []string{"CURRENT", "NAME", "URL"}, tab.Columns())
	rows := tab.Rows()
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"*", "acme", "https://a"}, rows[0]) // current marked
	assert.Equal(t, []string{"", "beta", "https://b"}, rows[1])
}

func TestResolveContextForServer(t *testing.T) {
	t.Parallel()

	t.Run("current context preferred over lexicographic", func(t *testing.T) {
		cfg := &config.Config{
			CurrentContext: "z-ctx",
			Contexts: map[string]config.Context{
				"a-ctx": {Server: "acme"},
				"z-ctx": {Server: "acme"},
			},
		}
		ctxName, matches := resolveContextForServer(cfg, "acme")
		assert.Equal(t, "z-ctx", ctxName)
		assert.Equal(t, []string{"a-ctx", "z-ctx"}, matches)
	})

	t.Run("lexicographic first when current targets another server", func(t *testing.T) {
		cfg := &config.Config{
			CurrentContext: "other",
			Contexts: map[string]config.Context{
				"other": {Server: "beta"},
				"b-ctx": {Server: "acme"},
				"a-ctx": {Server: "acme"},
			},
		}
		ctxName, matches := resolveContextForServer(cfg, "acme")
		assert.Equal(t, "a-ctx", ctxName)
		assert.Equal(t, []string{"a-ctx", "b-ctx"}, matches)
	})

	t.Run("empty when no matches", func(t *testing.T) {
		cfg := &config.Config{Contexts: map[string]config.Context{"other": {Server: "beta"}}}
		ctxName, matches := resolveContextForServer(cfg, "acme")
		assert.Empty(t, ctxName)
		assert.Empty(t, matches)
	})

	t.Run("matches list sorted", func(t *testing.T) {
		cfg := &config.Config{
			Contexts: map[string]config.Context{
				"z-ctx": {Server: "acme"},
				"a-ctx": {Server: "acme"},
				"m-ctx": {Server: "acme"},
			},
		}
		_, matches := resolveContextForServer(cfg, "acme")
		assert.Equal(t, []string{"a-ctx", "m-ctx", "z-ctx"}, matches)
	})
}

// TestServerSwitchCompleteArgsDelegatesToServerNames proves only the
// wiring: sort order and the missing/corrupt-config degrade-to-empty
// behavior are serverNames-level facts, pinned once in names_test.go
// (external review findings 8+9).
func TestServerSwitchCompleteArgsDelegatesToServerNames(t *testing.T) {
	t.Parallel()
	view := testView{srvNames: []string{"acme", "beta"}}
	got := serverSwitchCmd{}.CompleteArgs(context.Background(), view)
	assert.Equal(t, []string{"acme", "beta"}, got)
}
