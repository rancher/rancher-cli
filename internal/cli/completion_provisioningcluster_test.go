package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// provisioningClusterServer serves the provisioning.cattle.io/v1 Cluster list
// from the fleet-default namespace on the local management cluster, counting
// hits. A second path accepts an explicit namespace override so the
// --namespace flag test can verify a different namespace is used.
func provisioningClusterServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	handler := func(ns string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			switch ns {
			case "fleet-default":
				_, _ = w.Write([]byte(`{"data":[
					{"id":"fleet-default/my-rke2","metadata":{"name":"my-rke2","namespace":"fleet-default"}},
					{"id":"fleet-default/my-k3s","metadata":{"name":"my-k3s","namespace":"fleet-default"}}
				],"revision":"1"}`))
			default:
				_, _ = fmt.Fprintf(w, `{"data":[{"id":"%[1]s/ns-cluster","metadata":{"name":"ns-cluster","namespace":%[1]q}}],"revision":"1"}`, ns)
			}
		}
	}
	mux.HandleFunc("/v1/provisioning.cattle.io.clusters/fleet-default", handler("fleet-default"))
	mux.HandleFunc("/v1/provisioning.cattle.io.clusters/fleet-west", handler("fleet-west"))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestProvisioningClusterPositionalSiteFullCallback drives the real adaptEntry
// callback through BuildRoot/root.Run with a marked line, mirroring
// TestNodeDriverPositionalSiteFullCallback: network on → live provisioning-cluster
// names; network off → empty.
// NOT parallel: t.Setenv mutates env globals, setStoredConfig mutates storedConfig.
func TestProvisioningClusterPositionalSiteFullCallback(t *testing.T) {
	srv, _ := provisioningClusterServer(t)

	setup := func(t *testing.T, networkOn bool) {
		t.Helper()
		dir := t.TempDir()
		cfg := &config.Config{
			CurrentServer: "srv",
			Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:    config.CompletionConfig{Network: networkOn},
		}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		setStoredConfigForTest(t, cfg, dir)
		t.Setenv(config.EnvConfigDir, dir)
		t.Setenv(config.EnvToken, "mgmt-token")
		t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	}

	run := func(t *testing.T, words []string, currentWord string) string {
		t.Helper()
		origArgv, origWord, origProto := rawArgv, rawCurrentWord, rawCompleting
		t.Cleanup(func() {
			rawArgv = origArgv
			rawCurrentWord = origWord
			rawCompleting = origProto
		})
		scriptArgs := append([]string{"--rancher-completion-proto=2"}, words...)
		scriptArgs = append(scriptArgs, "--rancher-completion-cur="+currentWord, "--generate-shell-completion")
		CaptureArgv(scriptArgs)
		root, err := BuildRoot("test")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer = &out
		_ = root.Run(t.Context(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
		return out.String()
	}

	t.Run("delete provisioning-cluster positional → cluster names", func(t *testing.T) {
		setup(t, true)
		got := run(t, []string{"delete", "provisioning-cluster"}, "")
		assert.Contains(t, got, "my-rke2")
		assert.Contains(t, got, "my-k3s")
	})

	t.Run("get provisioning-cluster positional → cluster names", func(t *testing.T) {
		setup(t, true)
		got := run(t, []string{"get", "provisioning-cluster"}, "")
		assert.Contains(t, got, "my-rke2")
		assert.Contains(t, got, "my-k3s")
	})

	t.Run("network gate off → empty", func(t *testing.T) {
		setup(t, false)
		got := run(t, []string{"delete", "provisioning-cluster"}, "")
		assert.Empty(t, got)
	})
}
