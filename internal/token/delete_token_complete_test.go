package token_test

import (
	"context"
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

func (v testConfigView) ContextNames() []string   { return nil }
func (v testConfigView) ServerNames() []string    { return nil }
func (v testConfigView) CurrentContext() string   { return "" }
func (v testConfigView) CurrentServerURL() string { return "" }
func (v testConfigView) ConfigDir() string        { return v.dir }

var _ runtime.ConfigView = testConfigView{}

func TestDeleteTokenCompleteArgsReturnsEntryNames(t *testing.T) {
	// No keyring.MockInit: ReadOnlyCachedCreds reads a file, never keyring
	// (round-2 finding 9: keyring setup unnecessary for this code path).
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("k8s-prod", "rancher.example.com", "c-abc"),
		"tok", time.Now().Add(time.Hour),
		"k8s-prod", "rancher.example.com", "c-abc",
	))
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("k8s-staging", "rancher.example.com", "c-def"),
		"tok2", time.Now().Add(time.Hour),
		"k8s-staging", "rancher.example.com", "c-def",
	))
	cmd := token.NewDeleteTokenCommand(dir, store)
	sc, ok := cmd.(runtime.ShellCompleter)
	require.True(t, ok, "deleteTokenCmd must implement ShellCompleter")
	got := sc.CompleteArgs(context.Background(), testConfigView{dir: dir})
	assert.Equal(t, []string{"k8s-prod", "k8s-staging"}, got)
}

func TestDeleteTokenCompleteArgsExcludesCompositeKeys(t *testing.T) {
	// No keyring.MockInit: ReadOnlyCachedCreds reads a file (round-2 finding 9).
	t.Parallel()
	dir := t.TempDir()
	store := config.NewFileStore(dir)
	require.NoError(t, store.SetCachedCred(
		config.CacheKey("ctx", "host.example.com", "c-abc"),
		"tok", time.Now().Add(time.Hour),
		"ctx", "host.example.com", "c-abc",
	))
	cmd := token.NewDeleteTokenCommand(dir, store)
	sc := cmd.(runtime.ShellCompleter)
	got := sc.CompleteArgs(context.Background(), testConfigView{dir: dir})
	// Name field only ("ctx"), not the composite key.
	assert.Equal(t, []string{"ctx"}, got)
	for _, c := range got {
		assert.NotContains(t, c, "@", "composite keys must not be offered")
	}
}
