package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
)

// TestPositionalEnumCandidatesStatic pins MC-14/MC-15: a positional site
// whose ArgSpec carries an Enum is OWNED by the static layer — served
// index-aware and partial-filtered, with an unmatched partial owned but
// empty; an enum-less positional (the <name> site) and an out-of-range
// index are not owned.
func TestPositionalEnumCandidatesStatic(t *testing.T) {
	t.Parallel()
	d := runtime.Descriptor{
		Name: "create machine-config",
		Args: []runtime.ArgSpec{
			{Name: "kind", Type: "string", Required: true, Enum: []string{
				"amazonec2config", "azureconfig", "digitaloceanconfig",
				"harvesterconfig", "vmwarevsphereconfig",
			}},
			{Name: "name", Type: "string", Required: true},
		},
	}

	t.Run("index 0 empty partial → full enum, owned", func(t *testing.T) {
		t.Parallel()
		got, owned := positionalEnumCandidates(CompletionSite{Kind: SitePositional, Index: 0}, d)
		assert.True(t, owned)
		assert.Equal(t, []string{
			"amazonec2config", "azureconfig", "digitaloceanconfig",
			"harvesterconfig", "vmwarevsphereconfig",
		}, got)
	})

	t.Run("partial filters, owned", func(t *testing.T) {
		t.Parallel()
		got, owned := positionalEnumCandidates(CompletionSite{Kind: SitePositional, Index: 0, Partial: "di"}, d)
		assert.True(t, owned)
		assert.Equal(t, []string{"digitaloceanconfig"}, got)
	})

	t.Run("unmatched partial → owned with zero candidates", func(t *testing.T) {
		t.Parallel()
		got, owned := positionalEnumCandidates(CompletionSite{Kind: SitePositional, Index: 0, Partial: "x"}, d)
		assert.True(t, owned, "the site is static: an unmatched partial must not fall through")
		assert.Empty(t, got)
	})

	t.Run("name positional (index 1, no enum) → not owned", func(t *testing.T) {
		t.Parallel()
		got, owned := positionalEnumCandidates(CompletionSite{Kind: SitePositional, Index: 1}, d)
		assert.False(t, owned)
		assert.Nil(t, got)
	})

	t.Run("index past the declared args → not owned", func(t *testing.T) {
		t.Parallel()
		got, owned := positionalEnumCandidates(CompletionSite{Kind: SitePositional, Index: 2}, d)
		assert.False(t, owned)
		assert.Nil(t, got)
	})
}

// recordingCompleter counts CompleteArgs delegations (the layer that must
// NOT run when the static layer owns the site).
type recordingCompleter struct{ calls atomic.Int32 }

func (r *recordingCompleter) CompleteArgs(context.Context, runtime.ConfigView) []string {
	r.calls.Add(1)
	return []string{"from-completer"}
}

// TestPositionalEnumOwnershipStopsLowerLayers pins the ownership contract
// through dispatchSite: an unmatched partial on an enum-carrying positional
// terminates with zero candidates and ZERO lower-layer activity — no
// CompleteArgs delegation, no live fetch — while an enum-less positional
// still delegates.
func TestPositionalEnumOwnershipStopsLowerLayers(t *testing.T) {
	// NOT parallel: mutates storedConfig.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(srv.Close)
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())

	d := runtime.Descriptor{
		Name: "completion",
		Args: []runtime.ArgSpec{
			{Name: "shell", Type: "string", Required: true, Enum: runtime.ShellCompletionShells()},
		},
	}
	sc := &recordingCompleter{}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	got := dispatchSite(ctx, CompletionSite{Kind: SitePositional, Index: 0, Partial: "x"}, d, sc, nil, nil)
	assert.Empty(t, got, "unmatched partial on an owned site: zero candidates")
	assert.Zero(t, sc.calls.Load(), "CompleteArgs must not run for an owned site")
	assert.Zero(t, hits.Load(), "no live fetch for an owned site")

	got = dispatchSite(ctx, CompletionSite{Kind: SitePositional, Index: 1}, d, sc, nil, nil)
	assert.Equal(t, []string{"from-completer"}, got, "enum-less positional still delegates")
	assert.Equal(t, int32(1), sc.calls.Load())
}

// TestCompletionShellPositionalParity pins MC-14's free rider: the
// `completion <shell>` positional is served by the same static layer,
// partial-filtered, through dispatchSite with the network gate OFF and
// zero HTTP requests.
func TestCompletionShellPositionalParity(t *testing.T) {
	// NOT parallel: mutates storedConfig.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(srv.Close)
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: false},
	}
	setStoredConfig(cfg, t.TempDir())

	d := runtime.Descriptor{
		Name: "completion",
		Args: []runtime.ArgSpec{
			{Name: "shell", Type: "string", Required: true, Enum: runtime.ShellCompletionShells()},
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	got := dispatchSite(ctx, CompletionSite{Kind: SitePositional, Index: 0, Partial: "z"}, d, nil, nil, nil)
	assert.Equal(t, []string{"zsh"}, got)
	assert.Zero(t, hits.Load(), "static positional enums must cost zero requests")

	got = dispatchSite(ctx, CompletionSite{Kind: SitePositional, Index: 1}, d, nil, nil, nil)
	assert.Nil(t, got, "past the one declared arg: nothing")
}
