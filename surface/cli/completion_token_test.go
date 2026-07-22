package cli

import (
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zalando/go-keyring"
)

// NOTE: tests below mutate storedConfig (package global) and env — NOT parallel.
func TestResolveCompletionTokenEnvWins(t *testing.T) {
	keyring.MockInit()
	dir := t.TempDir()
	t.Setenv(config.EnvToken, "mytoken")
	cfg := &config.Config{
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv"}},
		Servers:        map[string]config.Server{"srv": {URL: "https://rancher.example.com"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfig(cfg, dir)
	// argTail nil: no --context flag override; env token must win (AC-4).
	cap := resolveCompletionToken(nil, "")
	assert.Equal(t, "mytoken", cap.Token)
	assert.False(t, cap.IDOnly)
}

func TestResolveCompletionTokenExecCredIDOnly(t *testing.T) {
	// NOT parallel: mutates storedConfig package global and env.
	t.Setenv(config.EnvToken, "")
	keyring.MockInit()
	dir := t.TempDir()
	// No env token, no file store token (CredentialStore not set to "file").
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("k8s-ctx", "rancher.example.com", "c-abc"),
		"exec-token", time.Now().Add(time.Hour),
		"k8s-ctx", "rancher.example.com", "c-abc",
	))
	cfg := &config.Config{
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv"}},
		Servers:        map[string]config.Server{"srv": {URL: "https://rancher.example.com"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfig(cfg, dir)
	cap := resolveCompletionToken(nil, "c-abc")
	assert.Equal(t, "exec-token", cap.Token)
	assert.True(t, cap.IDOnly)
}

func TestResolveCompletionTokenAmbiguousExecCredYieldsEmpty(t *testing.T) {
	// NOT parallel: mutates storedConfig package global and env.
	t.Setenv(config.EnvToken, "")
	keyring.MockInit()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	// Two entries for the same server+cluster → ambiguous.
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("ctx-a", "rancher.example.com", "c-abc"),
		"tok-a", time.Now().Add(time.Hour),
		"ctx-a", "rancher.example.com", "c-abc",
	))
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("ctx-b", "rancher.example.com", "c-abc"),
		"tok-b", time.Now().Add(time.Hour),
		"ctx-b", "rancher.example.com", "c-abc",
	))
	cfg := &config.Config{
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv"}},
		Servers:        map[string]config.Server{"srv": {URL: "https://rancher.example.com"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfig(cfg, dir)
	cap := resolveCompletionToken(nil, "c-abc")
	assert.Empty(t, cap.Token, "ambiguous exec-cred must yield empty")
}

// TestFileStoreTokenCrossServerURLOverride proves round-2 finding 3:
// RANCHER_CLI_URL pointing at server B must not send server A's token to B.
func TestFileStoreTokenCrossServerURLOverride(t *testing.T) {
	// NOT parallel: mutates storedConfig and env globals.
	t.Setenv(config.EnvToken, "")
	keyring.MockInit()
	dir := t.TempDir()
	// Config current server is server A (rancher-a.example.com).
	cfg := &config.Config{
		CurrentContext:  "ctx",
		Contexts:        map[string]config.Context{"ctx": {Server: "srv-a"}},
		Servers:         map[string]config.Server{"srv-a": {URL: "https://rancher-a.example.com"}},
		CredentialStore: "file",
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	// Write a file-store token for server A using the real API (round-3 finding 2:
	// config.WriteFileToken does not exist; use NewFileStore.SetToken instead).
	require.NoError(t, config.NewFileStore(dir).SetToken(config.ServerRef{Name: "srv-a"}, "token-for-a"))
	setStoredConfig(cfg, dir)

	// RANCHER_CLI_URL points at server B.
	t.Setenv(config.EnvURL, "https://rancher-b.example.com")

	// Must return empty: server A's token must not be sent to server B.
	cap := resolveCompletionToken(nil, "")
	assert.Empty(t, cap.Token, "round-2 finding 3: cross-server URL override must not send server A token to server B")
}
