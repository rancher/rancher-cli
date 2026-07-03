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

func TestContextSwitchPreservesProjectOnClusterOnlyInput(t *testing.T) {
	dir := t.TempDir()
	writeCtxConfig(t, dir)
	// Colon-less scope: change cluster only, keep project (matches RANCHER_CONTEXT).
	_, err := NewContextSwitch(dir).Execute(context.Background(),
		args{a: []string{"c-2"}}, safety.Effect{})
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "c-2", cfg.Contexts["acme"].Cluster)
	assert.Equal(t, "p-1", cfg.Contexts["acme"].Project) // preserved
}

func TestContextSwitchSetsBothWithColon(t *testing.T) {
	dir := t.TempDir()
	writeCtxConfig(t, dir)
	_, err := NewContextSwitch(dir).Execute(context.Background(),
		args{a: []string{"c-2:p-2"}}, safety.Effect{})
	require.NoError(t, err)
	cfg, _ := config.Load(config.FilePath(dir))
	assert.Equal(t, "c-2", cfg.Contexts["acme"].Cluster)
	assert.Equal(t, "p-2", cfg.Contexts["acme"].Project)
}

func TestContextSwitchGuardIsValidation(t *testing.T) {
	dir := t.TempDir()
	writeCtxConfig(t, dir)
	_, err := NewContextSwitch(dir).Execute(context.Background(), args{}, safety.Effect{})
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
