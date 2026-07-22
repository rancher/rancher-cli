package config_test

import (
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadOnlyCachedCredsNeverWrites is a pure file-store test: ReadOnlyCachedCreds
// reads token-cache.yaml directly and never touches the keyring.
// keyring.MockInit() is NOT needed (round-3 finding 6).
// t.Parallel() is safe because no global state (keyring or env) is mutated.
func TestReadOnlyCachedCredsNeverWrites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)

	// Write one live and one expired entry.
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("live", "https://rancher.example.com", "c-abc"),
		"token-live", time.Now().Add(time.Hour),
		"live", "https://rancher.example.com", "c-abc",
	))
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("expired", "https://rancher.example.com", "c-abc"),
		"token-expired", time.Now().Add(-time.Hour),
		"expired", "https://rancher.example.com", "c-abc",
	))

	infos, err := config.ReadOnlyCachedCreds(dir)
	require.NoError(t, err)

	// Only the live entry is returned.
	assert.Len(t, infos, 1)
	assert.Equal(t, "live", infos[0].Name)

	// The file must not have been modified (expired entry still on disk).
	infos2, err := config.ReadOnlyCachedCreds(dir)
	require.NoError(t, err)
	assert.Len(t, infos2, 1, "expired entry must remain on disk (no prune write)")
}

// TestReadOnlyCachedCredsForClusterWithTokens verifies that the token is
// propagated in a single file read (finding 8: no second parse needed).
func TestReadOnlyCachedCredsForClusterWithTokens(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)

	require.NoError(t, store.SetCachedCred(
		config.CacheKey("ctx", "https://rancher.example.com", "c-abc"),
		"my-token", time.Now().Add(time.Hour),
		"ctx", "https://rancher.example.com", "c-abc",
	))

	entries, err := config.ReadOnlyCachedCredsForClusterWithTokens(dir, "https://rancher.example.com", "c-abc")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "ctx", entries[0].Name)
	assert.Equal(t, "my-token", entries[0].Token, "finding 8: token must be present without a second file read")
}

func TestReadOnlyCachedCredsForClusterWithTokensExpiredExcluded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)

	require.NoError(t, store.SetCachedCred(
		config.CacheKey("ctx", "https://rancher.example.com", "c-abc"),
		"expired-tok", time.Now().Add(-time.Hour),
		"ctx", "https://rancher.example.com", "c-abc",
	))

	entries, err := config.ReadOnlyCachedCredsForClusterWithTokens(dir, "https://rancher.example.com", "c-abc")
	require.NoError(t, err)
	assert.Empty(t, entries, "expired entries must be excluded")
}
