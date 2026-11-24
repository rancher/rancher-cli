package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// managedChartServer is a test server that responds to list requests for the
// managedcharts of the Fleet workspaces fleet-default and fleet-local on the
// local management cluster.
func managedChartServer(t *testing.T) *httptest.Server {
	t.Helper()
	const base = "/k8s/clusters/local/apis/management.cattle.io/v3/namespaces/"
	mux := http.NewServeMux()
	mux.HandleFunc(base+"fleet-default/managedcharts", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"monitoring-default","namespace":"fleet-default"}},
			{"metadata":{"name":"logging-default","namespace":"fleet-default"}}
		]}`))
	})
	mux.HandleFunc(base+"fleet-local/managedcharts", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"monitoring-local","namespace":"fleet-local"}}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestManagedChartFullCallback drives the real completion callback through
// BuildRoot with the arguments a shell completion script passes, and asserts
// that get, patch and delete offer the names of the default workspace
// fleet-default, or of the workspace typed with --namespace, and that no
// names are offered with the network gate off. Not parallel: it sets the
// process environment and the captured command line.
func TestManagedChartFullCallback(t *testing.T) {
	srv := managedChartServer(t)

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

	complete := func(t *testing.T, words ...string) string {
		t.Helper()
		origArgv, origWord, origProto := rawArgv, rawCurrentWord, rawCompleting
		t.Cleanup(func() {
			rawArgv = origArgv
			rawCurrentWord = origWord
			rawCompleting = origProto
		})
		scriptArgs := append([]string{"--rancher-completion-proto=2"}, words...)
		scriptArgs = append(scriptArgs, "--rancher-completion-cur=", "--generate-shell-completion")
		CaptureArgv(scriptArgs)
		root, err := BuildRoot("test")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer = &out
		_ = root.Run(t.Context(), append([]string{"rancher"}, scriptArgs...))
		return out.String()
	}

	for _, verb := range []string{"get", "patch", "delete"} {
		t.Run(verb+" lists the names of fleet-default", func(t *testing.T) {
			setup(t, true)
			got := complete(t, verb, "managedchart")
			assert.Contains(t, got, "monitoring-default")
			assert.Contains(t, got, "logging-default")
			assert.NotContains(t, got, "monitoring-local")
		})
	}

	t.Run("get lists the names of the typed namespace", func(t *testing.T) {
		setup(t, true)
		got := complete(t, "get", "managedchart", "--namespace", "fleet-local")
		assert.Contains(t, got, "monitoring-local")
		assert.NotContains(t, got, "monitoring-default")
		assert.NotContains(t, got, "logging-default")
	})

	t.Run("network gate off offers nothing", func(t *testing.T) {
		setup(t, false)
		assert.Empty(t, complete(t, "get", "managedchart"))
	})
}
