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
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: "https://rancher.example.com"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfig(cfg, dir)
	cap := resolveCompletionToken("")
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
		config.CacheKey("k8s-ctx", "https://rancher.example.com", "c-abc"),
		"exec-token", time.Now().Add(time.Hour),
		"k8s-ctx", "https://rancher.example.com", "c-abc",
	))
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: "https://rancher.example.com"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfig(cfg, dir)
	cap := resolveCompletionToken("c-abc")
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
		config.CacheKey("ctx-a", "https://rancher.example.com", "c-abc"),
		"tok-a", time.Now().Add(time.Hour),
		"ctx-a", "https://rancher.example.com", "c-abc",
	))
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("ctx-b", "https://rancher.example.com", "c-abc"),
		"tok-b", time.Now().Add(time.Hour),
		"ctx-b", "https://rancher.example.com", "c-abc",
	))
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: "https://rancher.example.com"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfig(cfg, dir)
	cap := resolveCompletionToken("c-abc")
	assert.Empty(t, cap.Token, "ambiguous exec-cred must yield empty")
}

// TestResolveCompletionTokenInvalidServerURLYieldsEmpty is the final-re-review
// finding-1 repro: completion must return an empty capability for a configured
// server URL that fails HTTP(S) validation, never surfacing a stored record.
//
// Three sub-cases: unsupported scheme (ftp://), malformed URL (https://[::1),
// and empty hostname (https://:443). Each seeds both a file-store token and an
// exec-cache entry for the bad URL, then asserts the returned capability is empty.
func TestResolveCompletionTokenInvalidServerURLYieldsEmpty(t *testing.T) {
	cases := []struct {
		name      string
		serverURL string
	}{
		{"unsupported scheme ftp", "ftp://host"},
		{"malformed unclosed IPv6", "https://[::1"},
		{"empty hostname default port", "https://:443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// NOT parallel: mutates storedConfig and env globals.
			t.Setenv(config.EnvToken, "")
			keyring.MockInit()
			dir := t.TempDir()

			// Seed a file-store token for the server entry with the bad URL.
			fileStore := config.NewFileStore(dir)
			require.NoError(t, fileStore.SetToken(config.ServerRef{Name: "bad"}, "should-not-surface"))

			// Seed an exec-cache entry under the bad URL's identity.
			badID := config.CanonicalServerIdentity(tc.serverURL)
			require.NoError(t, fileStore.SetCachedCred(
				config.CacheKey("bad", badID, "c-1"),
				"also-should-not-surface", time.Now().Add(time.Hour),
				"bad", badID, "c-1",
			))

			cfg := &config.Config{
				CurrentServer:   "bad",
				Servers:         map[string]config.Server{"bad": {URL: tc.serverURL}},
				CredentialStore: "file",
			}
			cfg.SetPath(config.FilePath(dir))
			require.NoError(t, cfg.Write())
			setStoredConfig(cfg, dir)

			cap := resolveCompletionToken("c-1")
			assert.Empty(t, cap.Token,
				"invalid server URL %q must yield empty capability, not surface a stored credential",
				tc.serverURL)
		})
	}
}

// TestResolveCompletionTokenShimEntryPlusNativeResolvesNative is the failing-
// first test for round-4 finding 1 (AC-6 collision). A shim cache entry for
// the same server+cluster as a native entry must be excluded from the
// single-entry candidacy count so completion still resolves from the native
// entry (shim + 1 native → native).
func TestResolveCompletionTokenShimEntryPlusNativeResolvesNative(t *testing.T) {
	// NOT parallel: mutates storedConfig package global and env.
	t.Setenv(config.EnvToken, "")
	keyring.MockInit()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	// Native entry ("ctx-native").
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("ctx-native", "https://rancher.example.com", "c-abc"),
		"native-tok", time.Now().Add(time.Hour),
		"ctx-native", "https://rancher.example.com", "c-abc",
	))
	// Shim entry ("legacy:x") for the same server+cluster.
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("legacy:x", "https://rancher.example.com", "c-abc"),
		"shim-tok", time.Now().Add(time.Hour),
		"legacy:x", "https://rancher.example.com", "c-abc",
	))
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: "https://rancher.example.com"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfig(cfg, dir)
	cap := resolveCompletionToken("c-abc")
	assert.Equal(t, "native-tok", cap.Token,
		"shim entry must be excluded; native entry must resolve as the sole candidate")
	assert.True(t, cap.IDOnly)
}

// TestResolveCompletionTokenShimOnlyYieldsEmpty verifies the shim-only case:
// when only a legacy:-prefixed cache entry exists for a server+cluster, AC-6
// must yield an empty capability — the slot is kubectl's, not the operator's.
func TestResolveCompletionTokenShimOnlyYieldsEmpty(t *testing.T) {
	// NOT parallel: mutates storedConfig package global and env.
	t.Setenv(config.EnvToken, "")
	keyring.MockInit()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	// Only a shim entry — no native entry.
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("legacy:y", "https://rancher.example.com", "c-def"),
		"shim-only-tok", time.Now().Add(time.Hour),
		"legacy:y", "https://rancher.example.com", "c-def",
	))
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: "https://rancher.example.com"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	setStoredConfig(cfg, dir)
	cap := resolveCompletionToken("c-def")
	assert.Empty(t, cap.Token,
		"shim-only entry must yield empty capability (not a completion candidate)")
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
		CurrentServer:   "srv-a",
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
	cap := resolveCompletionToken("")
	assert.Empty(t, cap.Token, "round-2 finding 3: cross-server URL override must not send server A token to server B")
}
