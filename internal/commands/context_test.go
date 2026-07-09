package commands

import (
	"context"
	"testing"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/ui"
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
	_, err := NewContextSet(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{b: map[string]bool{"current": true}, s: map[string]string{"cluster": "c-2"}}, ui.Discard())
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "c-2", cfg.Contexts["acme"].Cluster)
	assert.Equal(t, "p-1", cfg.Contexts["acme"].Project) // preserved (no --project)
}

func TestConfigSetContextSetsBoth(t *testing.T) {
	dir := t.TempDir()
	writeCtxConfig(t, dir)
	_, err := NewContextSet(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{b: map[string]bool{"current": true}, s: map[string]string{"cluster": "c-2", "project": "p-2"}}, ui.Discard())
	require.NoError(t, err)
	cfg, _ := config.Load(config.FilePath(dir))
	assert.Equal(t, "c-2", cfg.Contexts["acme"].Cluster)
	assert.Equal(t, "p-2", cfg.Contexts["acme"].Project)
}

// Exact reviewer repro: `rancher --project global-proj --config <dir> config
// set-context --current --cluster c-1` must not silently write an empty
// project — the global --project flag is shadowed by the (empty) local one.
func TestConfigSetContextRejectsShadowedGlobalProject(t *testing.T) {
	dir := t.TempDir()
	writeCtxConfig(t, dir)
	config.SetContextOverride("", "global-proj")
	t.Cleanup(func() { config.SetContextOverride("", "") })

	_, err := NewContextSet(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{b: map[string]bool{"current": true}, s: map[string]string{"cluster": "c-1"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "p-1", cfg.Contexts["acme"].Project, "must not overwrite with an empty project")
}

func TestConfigSetContextRejectsShadowedGlobalContext(t *testing.T) {
	dir := t.TempDir()
	writeCtxConfig(t, dir)
	config.SetContextOverride("c-global:p-global", "")
	t.Cleanup(func() { config.SetContextOverride("", "") })

	_, err := NewContextSet(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{b: map[string]bool{"current": true}, s: map[string]string{"project": "p-2"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestConfigSetContextUnaffectedWithoutGlobalOverride(t *testing.T) {
	dir := t.TempDir()
	writeCtxConfig(t, dir)
	_, err := NewContextSet(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{b: map[string]bool{"current": true}, s: map[string]string{"cluster": "c-2"}}, ui.Discard())
	require.NoError(t, err)
}

func TestConfigSetContextRequiresScopeFlag(t *testing.T) {
	dir := t.TempDir()
	writeCtxConfig(t, dir)
	// No --cluster/--project → validation error, not a silent no-op write.
	_, err := NewContextSet(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{b: map[string]bool{"current": true}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

func TestConfigSetContextCurrentIsOptional(t *testing.T) {
	dir := t.TempDir()
	writeCtxConfig(t, dir)
	// --current is optional (Phase 1 targets the current context); a scope flag
	// alone succeeds. This keeps the Descriptor honest: --current is not required.
	_, err := NewContextSet(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{s: map[string]string{"cluster": "c-2"}}, ui.Discard())
	require.NoError(t, err)
	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "c-2", cfg.Contexts["acme"].Cluster)
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

	_, err := NewContextUse(dir).(runtime.ReadCommand).Execute(context.Background(), args{a: []string{"beta"}}, ui.Discard())
	require.NoError(t, err)
	got, _ := config.Load(config.FilePath(dir))
	assert.Equal(t, "beta", got.CurrentContext)

	_, err = NewContextUse(dir).(runtime.ReadCommand).Execute(context.Background(), args{a: []string{"nope"}}, ui.Discard())
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

	_, err := NewContextCurrent(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
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

	_, err := NewContextSet(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{b: map[string]bool{"current": true}, s: map[string]string{"cluster": "c-2", "project": "p-2"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeContextNotSet, ce.Code)

	cfgAfter, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Empty(t, cfgAfter.Contexts, "config should not be corrupted with zero Context{}")
}
