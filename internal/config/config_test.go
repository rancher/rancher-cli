package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteThenLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := FilePath(dir)
	c := &Config{
		CurrentContext: "prod",
		Contexts:       map[string]Context{"prod": {Server: "acme", Cluster: "c-x", Project: "p-y"}},
		Servers:        map[string]Server{"acme": {URL: "https://rancher.example.com", CA: "PEM"}},
		path:           path,
	}
	require.NoError(t, c.Write())

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())

	got, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "prod", got.CurrentContext)
	assert.Equal(t, "https://rancher.example.com", got.Servers["acme"].URL)
}

func TestLoadMissingFileReturnsEmpty(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	require.NoError(t, err)
	assert.Empty(t, got.Servers)
}

func TestCurrentServerUnsetIsContextNotSet(t *testing.T) {
	_, _, err := (&Config{}).CurrentServer()
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeContextNotSet, ce.Code)
}

func TestResolveDirPrecedence(t *testing.T) {
	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("RANCHER_CONFIG_DIR", "/env/dir")
		got, err := ResolveDir("/flag/dir")
		require.NoError(t, err)
		assert.Equal(t, "/flag/dir", got)
	})
	t.Run("env over xdg", func(t *testing.T) {
		t.Setenv("RANCHER_CONFIG_DIR", "/env/dir")
		t.Setenv("XDG_CONFIG_HOME", "/xdg")
		got, err := ResolveDir("")
		require.NoError(t, err)
		assert.Equal(t, "/env/dir", got)
	})
	t.Run("xdg when no env", func(t *testing.T) {
		t.Setenv("RANCHER_CONFIG_DIR", "")
		t.Setenv("XDG_CONFIG_HOME", "/xdg")
		got, err := ResolveDir("")
		require.NoError(t, err)
		assert.Equal(t, "/xdg/rancher", got)
	})
}
