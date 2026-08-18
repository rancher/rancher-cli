package commands

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigPathPrintsResolvedPathAndExistsFalseWhenNoFile(t *testing.T) {
	dir := t.TempDir()
	want := config.FilePath(dir)

	res, err := NewConfigPath(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	require.NoError(t, err)
	assert.Equal(t, "ConfigPath", res.Kind)
	assert.Equal(t, want+"\n", res.Text, "Text carries the path plus a trailing newline")

	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, want, data["path"])
	assert.Equal(t, false, data["exists"], "prints the path even though the config file does not exist yet")
}

func TestConfigPathExistsTrueWhenFilePresent(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Servers: map[string]config.Server{}}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	res, err := NewConfigPath(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	require.NoError(t, err)

	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, data["exists"])
}
