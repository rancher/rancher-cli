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
