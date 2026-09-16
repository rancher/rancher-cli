package token_test

import (
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testConfigView is a minimal ConfigView for token package tests.
type testConfigView struct {
	dir string
}

func (v testConfigView) ContextNames() []string { return nil }
func (v testConfigView) ServerNames() []string  { return nil }
func (v testConfigView) ConfigDir() string      { return v.dir }

var _ runtime.ConfigView = testConfigView{}

func TestDeleteTokenCompleteArgsReturnsEntryNames(t *testing.T) {
	// No keyring.MockInit: ReadOnlyCachedCreds reads a file, never keyring
	// (round-2 finding 9: keyring setup unnecessary for this code path).
	t.Parallel()
	cacheDir := t.TempDir()
	store := config.NewFileStore(cacheDir)
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("k8s-prod", "rancher.example.com", "c-abc"),
		"tok", time.Now().Add(time.Hour),
		"k8s-prod", "rancher.example.com", "c-abc",
		config.CacheBinding{Dir: cacheDir},
	))
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("k8s-staging", "rancher.example.com", "c-def"),
		"tok2", time.Now().Add(time.Hour),
		"k8s-staging", "rancher.example.com", "c-def",
		config.CacheBinding{Dir: cacheDir},
	))
	// Command constructed with EMPTY dir (finding 2: must not fall back to d.dir).
	// Candidates must come from the view's dir, not the command's own dir.
	cmd := token.NewDeleteTokenCommand("", store)
	sc, ok := cmd.(runtime.ShellCompleter)
	require.True(t, ok, "deleteTokenCmd must implement ShellCompleter")
	got := sc.CompleteArgs(t.Context(), testConfigView{dir: cacheDir})
	assert.Equal(t, []string{"k8s-prod", "k8s-staging"}, got)
}

func TestDeleteTokenCompleteArgsExcludesCompositeKeys(t *testing.T) {
	// No keyring.MockInit: ReadOnlyCachedCreds reads a file (round-2 finding 9).
	t.Parallel()
	cacheDir := t.TempDir()
	store := config.NewFileStore(cacheDir)
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("ctx", "host.example.com", "c-abc"),
		"tok", time.Now().Add(time.Hour),
		"ctx", "host.example.com", "c-abc",
		config.CacheBinding{Dir: cacheDir},
	))
	// Command constructed with EMPTY dir (finding 2: proving candidates come from view).
	cmd := token.NewDeleteTokenCommand("", store)
	sc := cmd.(runtime.ShellCompleter)
	got := sc.CompleteArgs(t.Context(), testConfigView{dir: cacheDir})
	// Name field only ("ctx"), not the composite key.
	assert.Equal(t, []string{"ctx"}, got)
	for _, c := range got {
		assert.NotContains(t, c, "@", "composite keys must not be offered")
	}
}

// TestDeleteTokenCompleteArgsEmptyViewDirYieldsNil proves the d.dir fallback
// is gone: when the view supplies no dir, CompleteArgs returns nil (finding 2).
func TestDeleteTokenCompleteArgsEmptyViewDirYieldsNil(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	store := config.NewFileStore(cacheDir)
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("ctx", "rancher.example.com", "c-abc"),
		"tok", time.Now().Add(time.Hour),
		"ctx", "rancher.example.com", "c-abc",
		config.CacheBinding{Dir: cacheDir},
	))
	// Command has a real dir but view has empty dir - without d.dir fallback,
	// must return nil (not the entries from cacheDir).
	cmd := token.NewDeleteTokenCommand(cacheDir, store)
	sc := cmd.(runtime.ShellCompleter)
	got := sc.CompleteArgs(t.Context(), testConfigView{dir: ""})
	assert.Nil(t, got, "finding 2: empty view dir must yield nil, not fall back to d.dir")
}
