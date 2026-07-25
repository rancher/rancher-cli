package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createMachineConfigDescriptor() runtime.Descriptor {
	return runtime.Descriptor{
		Name: "create machine-config",
		Flags: []runtime.ArgSpec{
			{Name: "region", Type: "string"},
			{Name: "set", Type: "stringSlice"},
		},
	}
}

// TestMachineConfigSetKeyCandidates pins MC-16: kind-scoped key
// candidates = Known − Credential − Curated, as key= continuation tokens,
// in space and equals form, only for create machine-config's --set — and
// the case OWNS that site for every kind (owned even when empty).
func TestMachineConfigSetKeyCandidates(t *testing.T) {
	t.Parallel()
	d := createMachineConfigDescriptor()

	t.Run("DO kind, space form → known minus credential minus curated", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "set", FlagSpelling: "--set"}
		got, owned := machineConfigSetKeyCandidates(site, d, []string{"digitaloceanconfig", "web-pool", "--set"})
		assert.True(t, owned)
		assert.Equal(t, []string{
			"backups=", "sshKeyContents=", "sshKeyFingerprint=",
			"sshPort=", "sshUser=", "userdata=",
		}, got)
		assert.Contains(t, got, "backups=", "a valid candidate is present")
	})

	t.Run("partial filters", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "set", FlagSpelling: "--set", Partial: "ss"}
		got, owned := machineConfigSetKeyCandidates(site, d, []string{"digitaloceanconfig", "web-pool", "--set"})
		assert.True(t, owned)
		assert.Equal(t, []string{"sshKeyContents=", "sshKeyFingerprint=", "sshPort=", "sshUser="}, got)
	})

	t.Run("equals form → full --set=key= tokens", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "set", FlagSpelling: "--set", EqualsForm: true, Partial: "u"}
		got, owned := machineConfigSetKeyCandidates(site, d, []string{"digitaloceanconfig", "web-pool"})
		assert.True(t, owned)
		assert.Equal(t, []string{"--set=userdata="}, got)
	})

	t.Run("every curated key pinned absent", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "set", FlagSpelling: "--set"}
		got, _ := machineConfigSetKeyCandidates(site, d, []string{"digitaloceanconfig", "web-pool", "--set"})
		f, ok := kinds.MachineConfigFieldsFor("digitaloceanconfig")
		require.True(t, ok)
		require.NotEmpty(t, f.Curated)
		for _, cur := range f.Curated {
			assert.NotContains(t, got, cur+"=",
				"curated key %q must never be suggested — MC-7 rejects it via --set", cur)
		}
	})

	t.Run("every credential key pinned absent", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "set", FlagSpelling: "--set"}
		got, _ := machineConfigSetKeyCandidates(site, d, []string{"digitaloceanconfig", "web-pool", "--set"})
		f, ok := kinds.MachineConfigFieldsFor("digitaloceanconfig")
		require.True(t, ok)
		require.NotEmpty(t, f.Credential)
		for _, cred := range f.Credential {
			assert.NotContains(t, got, cred+"=", "credential key %q must never be suggested", cred)
		}
	})

	t.Run("unmatched partial → owned with zero candidates", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "set", FlagSpelling: "--set", Partial: "zzz"}
		got, owned := machineConfigSetKeyCandidates(site, d, []string{"digitaloceanconfig", "x", "--set"})
		assert.True(t, owned, "the site is static: an unmatched partial must not fall through")
		assert.Empty(t, got)
	})

	t.Run("typing the value (partial contains =) → owned, empty", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "set", Partial: "backups=tr"}
		got, owned := machineConfigSetKeyCandidates(site, d, []string{"digitaloceanconfig", "x", "--set"})
		assert.True(t, owned)
		assert.Empty(t, got)
	})

	t.Run("uncurated kind → owned, empty", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "set", FlagSpelling: "--set"}
		got, owned := machineConfigSetKeyCandidates(site, d, []string{"azureconfig", "x", "--set"})
		assert.True(t, owned)
		assert.Empty(t, got)
	})

	t.Run("no kind in the tail → owned, empty", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "set", FlagSpelling: "--set"}
		got, owned := machineConfigSetKeyCandidates(site, d, []string{"--set"})
		assert.True(t, owned)
		assert.Empty(t, got)
	})

	t.Run("unknown kind token → owned, empty", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "set", FlagSpelling: "--set"}
		got, owned := machineConfigSetKeyCandidates(site, d, []string{"nonesuch", "x", "--set"})
		assert.True(t, owned)
		assert.Empty(t, got)
	})

	t.Run("other command or other flag → not owned", func(t *testing.T) {
		t.Parallel()
		_, owned := machineConfigSetKeyCandidates(
			CompletionSite{Kind: SiteFlagValue, FlagName: "region"}, d,
			[]string{"digitaloceanconfig", "x"})
		assert.False(t, owned)
		_, owned = machineConfigSetKeyCandidates(
			CompletionSite{Kind: SiteFlagValue, FlagName: "set"},
			runtime.Descriptor{Name: "create cluster"},
			[]string{"digitaloceanconfig", "x"})
		assert.False(t, owned)
	})
}

// TestMachineConfigSetKeysServedWithoutNetwork pins MC-16's static
// position: compiled-in metadata, served through dispatchSite with the
// network gate OFF and zero HTTP requests.
func TestMachineConfigSetKeysServedWithoutNetwork(t *testing.T) {
	// NOT parallel: mutates storedConfig.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(srv.Close)
	cfg := &config.Config{
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv"}},
		Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:     config.CompletionConfig{Network: false},
	}
	setStoredConfig(cfg, t.TempDir())

	d := createMachineConfigDescriptor()
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "set", FlagSpelling: "--set"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := dispatchSite(ctx, site, d, nil, nil, []string{"digitaloceanconfig", "web-pool", "--set"})
	assert.Contains(t, got, "backups=")
	assert.Zero(t, hits.Load(), "compiled-in --set keys must cost zero requests")
}

// TestMachineConfigSetSiteOwnershipStopsLiveLayer pins the ownership
// contract through dispatchSite with the network gate ON: an unmatched
// --set partial terminates with zero candidates and zero HTTP requests —
// the fully static site never reaches the live layer.
func TestMachineConfigSetSiteOwnershipStopsLiveLayer(t *testing.T) {
	// NOT parallel: mutates storedConfig.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(srv.Close)
	cfg := &config.Config{
		CurrentContext: "ctx",
		Contexts:       map[string]config.Context{"ctx": {Server: "srv"}},
		Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:     config.CompletionConfig{Network: true},
	}
	setStoredConfig(cfg, t.TempDir())

	d := createMachineConfigDescriptor()
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "set", FlagSpelling: "--set", Partial: "zzz"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := dispatchSite(ctx, site, d, nil, nil, []string{"digitaloceanconfig", "web-pool", "--set"})
	assert.Empty(t, got)
	assert.Zero(t, hits.Load(), "an owned static site must never reach the live layer")
}
