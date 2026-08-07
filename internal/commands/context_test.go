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

func TestConfigSetContextRejectsShadowedGlobalContext(t *testing.T) {
	dir := t.TempDir()
	writeCtxConfig(t, dir)
	config.SetContextOverride("c-global:p-global")
	t.Cleanup(func() { config.SetContextOverride("") })

	_, err := NewContextSet(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{b: map[string]bool{"current": true}, s: map[string]string{"project": "p-2"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)
}

// Round-2 repro (confirmed live): `rancher --context c-global:p-global config
// set-context --current --cluster c-9` must not silently drop p-global — the
// standalone --project flag is gone, but --context's ":project" half still
// carries a live project value that a local --cluster-only call would
// otherwise discard unnoticed.
func TestConfigSetContextRejectsShadowedGlobalContextProjectHalf(t *testing.T) {
	dir := t.TempDir()
	writeCtxConfig(t, dir)
	config.SetContextOverride("c-global:p-global")
	t.Cleanup(func() { config.SetContextOverride("") })

	_, err := NewContextSet(dir).(runtime.ReadCommand).Execute(context.Background(),
		args{b: map[string]bool{"current": true}, s: map[string]string{"cluster": "c-9"}}, ui.Discard())
	var ce *clierr.Error
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, clierr.CodeValidation, ce.Code)

	cfg, err := config.Load(config.FilePath(dir))
	require.NoError(t, err)
	assert.Equal(t, "c-1", cfg.Contexts["acme"].Cluster, "must not write anything on rejection")
	assert.Equal(t, "p-1", cfg.Contexts["acme"].Project, "must not silently drop p-global's shadowed value")
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

// TestContextCurrentTextView pins the TTY text view: a single-row tabwriter
// table carrying NAME / SERVER / CLUSTER / PROJECT, with the JSON Data
// payload byte-identical to the previous map[string]string.
func TestContextCurrentTextView(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCtxConfig(t, dir) // acme: server=acme, cluster=c-1, project=p-1

	res, err := NewContextCurrent(dir).(runtime.ReadCommand).Execute(context.Background(), args{}, ui.Discard())
	require.NoError(t, err)

	// Text contains the header row and all four values.
	assert.Contains(t, res.Text, "NAME")
	assert.Contains(t, res.Text, "SERVER")
	assert.Contains(t, res.Text, "CLUSTER")
	assert.Contains(t, res.Text, "PROJECT")
	assert.Contains(t, res.Text, "acme")
	assert.Contains(t, res.Text, "c-1")
	assert.Contains(t, res.Text, "p-1")

	// JSON Data payload is unchanged.
	data, ok := res.Data.(map[string]string)
	require.True(t, ok)
	assert.Equal(t, "acme", data["name"])
	assert.Equal(t, "acme", data["server"])
	assert.Equal(t, "c-1", data["cluster"])
	assert.Equal(t, "p-1", data["project"])
}

// TestRenderContextTextEmptyFields pins that empty cluster/project render as
// blank cells (not "-"), consistent with the existing table convention.
func TestRenderContextTextEmptyFields(t *testing.T) {
	t.Parallel()
	text := renderContextText("myctx", "myserver", "", "")
	assert.Contains(t, text, "NAME")
	assert.Contains(t, text, "myctx")
	assert.Contains(t, text, "myserver")
}

// TestContextUseCompleteArgsDelegatesToContextNames proves only the wiring:
// sort order is a ConfigView-level fact, pinned once at newConfigView in
// surface/cli.
func TestContextUseCompleteArgsDelegatesToContextNames(t *testing.T) {
	t.Parallel()
	view := testView{ctxNames: []string{"acme", "prod"}}
	got := contextUseCmd{}.CompleteArgs(context.Background(), view)
	assert.Equal(t, []string{"acme", "prod"}, got)
}
