package commands

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeCtxConfig(t *testing.T, dir string) {
	t.Helper()
	cfg := &config.Config{
		CurrentContext: "acme",
		Contexts:       map[string]config.Context{"acme": {Server: "acme", Cluster: "c-1", Project: "p-1"}},
		Servers:        map[string]config.Server{"acme": {URL: "https://r.ex"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
}

func TestConfigSetContextPreservesProjectOnClusterOnly(t *testing.T) {
	dir := t.TempDir()
	writeCtxConfig(t, dir)
	_, err := NewContextSet(dir).Execute(context.Background(),
		args{b: map[string]bool{"current": true}, s: map[string]string{"cluster": "c-2"}}, safety.Effect{})
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "c-2", cfg.Contexts["acme"].Cluster)
	assert.Equal(t, "p-1", cfg.Contexts["acme"].Project) // preserved (no --project)
}

func TestConfigSetContextSetsBoth(t *testing.T) {
	dir := t.TempDir()
	writeCtxConfig(t, dir)
	_, err := NewContextSet(dir).Execute(context.Background(),
		args{b: map[string]bool{"current": true}, s: map[string]string{"cluster": "c-2", "project": "p-2"}}, safety.Effect{})
	require.NoError(t, err)
	cfg, _ := config.Load(config.FilePath(dir))
	assert.Equal(t, "c-2", cfg.Contexts["acme"].Cluster)
	assert.Equal(t, "p-2", cfg.Contexts["acme"].Project)
}

func TestConfigSetContextRequiresCurrent(t *testing.T) {
	dir := t.TempDir()
	writeCtxConfig(t, dir)
	_, err := NewContextSet(dir).Execute(context.Background(),
		args{s: map[string]string{"cluster": "c-2"}}, safety.Effect{})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestContextUseActivatesNamedContext(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentContext: "acme",
		Contexts: map[string]config.Context{
			"acme": {Server: "acme"}, "beta": {Server: "beta"},
		},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	_, err := NewContextUse(dir).Execute(context.Background(), args{a: []string{"beta"}}, safety.Effect{})
	require.NoError(t, err)
	got, _ := config.Load(config.FilePath(dir))
	assert.Equal(t, "beta", got.CurrentContext)

	_, err = NewContextUse(dir).Execute(context.Background(), args{a: []string{"nope"}}, safety.Effect{})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeNotFound, ce.Code)
}

func TestContextCurrentMissingContextKey(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentContext: "acme",
		Contexts:       map[string]config.Context{},
		Servers:        map[string]config.Server{"acme": {URL: "https://r.ex"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	_, err := NewContextCurrent(dir).Execute(context.Background(), args{}, safety.Effect{})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeContextNotSet, ce.Code)
	assert.Contains(t, ce.Error(), "current context \"acme\" not found")
}

func TestConfigSetContextMissingContextKeyDoesNotCorruptConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		CurrentContext: "acme",
		Contexts:       map[string]config.Context{},
		Servers:        map[string]config.Server{"acme": {URL: "https://r.ex"}},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())

	_, err := NewContextSet(dir).Execute(context.Background(),
		args{b: map[string]bool{"current": true}, s: map[string]string{"cluster": "c-2", "project": "p-2"}}, safety.Effect{})
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeContextNotSet, ce.Code)

	cfgAfter, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Empty(t, cfgAfter.Contexts, "config should not be corrupted with zero Context{}")
}
