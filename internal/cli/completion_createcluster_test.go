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

// TestMachineConfigPrefixCandidatesStatic asserts the compiled-in `<kind>/`
// prefix layer: served for create cluster's --machine-config when the typed
// value has no "/" yet, in both space and equals form, and only there.
func TestMachineConfigPrefixCandidatesStatic(t *testing.T) {
	t.Parallel()
	create := runtime.Descriptor{Name: "create cluster"}

	t.Run("space-form empty partial → five kind/ prefixes", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config", FlagSpelling: "--machine-config"}
		got := machineConfigPrefixCandidates(site, create)
		assert.Equal(t, []string{
			"amazonec2config/", "azureconfig/", "digitaloceanconfig/",
			"harvesterconfig/", "vmwarevsphereconfig/",
		}, got)
	})

	t.Run("equals-form → full --machine-config=kind/ tokens", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config", FlagSpelling: "--machine-config", EqualsForm: true, Partial: "ama"}
		got := machineConfigPrefixCandidates(site, create)
		assert.Equal(t, []string{
			"--machine-config=amazonec2config/", "--machine-config=azureconfig/",
			"--machine-config=digitaloceanconfig/", "--machine-config=harvesterconfig/",
			"--machine-config=vmwarevsphereconfig/",
		}, got)
	})

	t.Run("partial with a slash → nil (the live layer owns kind/partial)", func(t *testing.T) {
		t.Parallel()
		site := CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config", Partial: "amazonec2config/cf"}
		assert.Nil(t, machineConfigPrefixCandidates(site, create))
	})

	t.Run("other flag or other command → nil", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, machineConfigPrefixCandidates(
			CompletionSite{Kind: SiteFlagValue, FlagName: "cloud-credential"}, create))
		assert.Nil(t, machineConfigPrefixCandidates(
			CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config"},
			runtime.Descriptor{Name: "get pod"}))
	})
}

// TestMachineConfigPrefixesServedWithoutNetwork asserts the static layer's
// position BEFORE the network gate: the prefixes are compiled-in kind-table
// literals (not server data), so they are served with completion.network
// off and zero HTTP requests - same doctrine as static enums.
func TestMachineConfigPrefixesServedWithoutNetwork(t *testing.T) {
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
		Name:  "create cluster",
		Flags: []runtime.ArgSpec{{Name: "machine-config", Type: "string"}},
	}
	site := CompletionSite{Kind: SiteFlagValue, FlagName: "machine-config", FlagSpelling: "--machine-config"}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	got := dispatchSite(ctx, site, d, nil, nil, []string{"node-1", "--machine-config"})
	assert.Contains(t, got, "amazonec2config/")
	assert.Len(t, got, 5)
	assert.Zero(t, hits.Load(), "compiled-in prefixes must cost zero requests")
}
