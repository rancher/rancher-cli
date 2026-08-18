package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteThenLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := FilePath(dir)
	c := &Config{
		CurrentServer: "acme",
		Servers:       map[string]Server{"acme": {URL: "https://rancher.example.com", CA: "PEM"}},
		path:          path,
	}
	require.NoError(t, c.Write())

	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		// On Windows os.FileInfo.Mode() synthesizes mode bits regardless of
		// the real NTFS DACL (hardenACL is the enforcement there), so the
		// 0600 assertion is meaningless on that platform.
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}

	got, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "acme", got.CurrentServer)
	assert.Equal(t, "https://rancher.example.com", got.Servers["acme"].URL)
}

func TestReadOnlyFieldRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := FilePath(dir)
	c := &Config{ReadOnly: true, Servers: map[string]Server{}, path: path}
	require.NoError(t, c.Write())

	got, err := Load(path)
	require.NoError(t, err)
	assert.True(t, got.ReadOnly)
}

func TestLoadMissingFileReturnsEmpty(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	require.NoError(t, err)
	assert.Empty(t, got.Servers)
}

func TestCurrentServerEntryUnsetIsNotLoggedIn(t *testing.T) {
	_, _, err := (&Config{}).CurrentServerEntry()
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotLoggedIn, ce.Code)
}

// TestCompletionTimeoutWithRaw pins the floor/cap/default contract for
// CompletionTimeoutWithRaw, and indirectly EffectiveCompletionTimeout (now a
// thin wrapper). Covers the pre-existing gap noted in the review: the contract
// was previously only exercised indirectly via set_completion_test.go.
func TestCompletionTimeoutWithRaw(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		effective time.Duration
		diverges  bool
	}{
		{"empty uses default", "", CompletionTimeoutDefault, false},
		{"in-range is unchanged", "1s", time.Second, false},
		{"non-canonical equal suppressed", "1000ms", time.Second, false},
		{"at cap is unchanged", "2s", CompletionTimeoutMax, false},
		{"at floor is unchanged", "500ms", CompletionTimeoutDefault, false},
		{"over cap is clamped", "10s", CompletionTimeoutMax, true},
		{"unparseable falls to default", "bad", CompletionTimeoutDefault, true},
		{"zero falls to default", "0", CompletionTimeoutDefault, true},
		{"negative falls to default", "-1s", CompletionTimeoutDefault, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Completion: CompletionConfig{Timeout: tc.raw}}
			eff, raw, div := CompletionTimeoutWithRaw(cfg)
			assert.Equal(t, tc.effective, eff)
			assert.Equal(t, tc.raw, raw)
			assert.Equal(t, tc.diverges, div)
			// EffectiveCompletionTimeout must agree — it is now a thin wrapper.
			assert.Equal(t, tc.effective, EffectiveCompletionTimeout(cfg))
		})
	}

	t.Run("nil config uses default no divergence", func(t *testing.T) {
		eff, raw, div := CompletionTimeoutWithRaw(nil)
		assert.Equal(t, CompletionTimeoutDefault, eff)
		assert.Equal(t, "", raw)
		assert.False(t, div)
	})
}

func TestResolveDirPrecedence(t *testing.T) {
	t.Run("flag wins", func(t *testing.T) {
		t.Setenv("RANCHER_CLI_CONFIG_DIR", "/env/dir")
		got, err := ResolveDir("/flag/dir")
		require.NoError(t, err)
		assert.Equal(t, "/flag/dir", got)
	})
	t.Run("env over xdg", func(t *testing.T) {
		t.Setenv("RANCHER_CLI_CONFIG_DIR", "/env/dir")
		t.Setenv("XDG_CONFIG_HOME", "/xdg")
		got, err := ResolveDir("")
		require.NoError(t, err)
		assert.Equal(t, "/env/dir", got)
	})
	t.Run("xdg when no env", func(t *testing.T) {
		t.Setenv("RANCHER_CLI_CONFIG_DIR", "")
		t.Setenv("XDG_CONFIG_HOME", "/xdg")
		got, err := ResolveDir("")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join("/xdg", "rancher"), got)
	})
}
