package commands

import (
	"bytes"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/rancher/rancher-cli/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigPathPrintsResolvedPathAndExistsFalseWhenNoFile(t *testing.T) {
	dir := t.TempDir()
	want := config.FilePath(dir)

	res, err := NewConfigPath(dir).(runtime.ReadCommand).Execute(t.Context(), args{}, ui.Discard())
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

	res, err := NewConfigPath(dir).(runtime.ReadCommand).Execute(t.Context(), args{}, ui.Discard())
	require.NoError(t, err)

	data, ok := res.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, data["exists"])
}

// TestConfigPathRendersByteIdenticalUnderColor pins the exemption: config
// path hand-builds Text directly (the bare, copyable path) and sets no
// StyledText, so --color=always renders byte-identical to plain.
func TestConfigPathRendersByteIdenticalUnderColor(t *testing.T) {
	dir := t.TempDir()
	cmd := NewConfigPath(dir)

	var plain, colored bytes.Buffer
	_, ce := runtime.Run(t.Context(), cmd, args{}, runtime.IO{Out: &plain, Err: new(bytes.Buffer), Format: ""}, safety.Binding{})
	require.Nil(t, ce)
	_, ce = runtime.Run(t.Context(), cmd, args{},
		runtime.IO{Out: &colored, Err: new(bytes.Buffer), Format: "", Color: "always", OutTTY: true}, safety.Binding{})
	require.Nil(t, ce)

	assert.Equal(t, plain.String(), colored.String())
	assert.NotContains(t, colored.String(), "\x1b[")
}
