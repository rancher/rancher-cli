package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authConfigNameServer is a test server that responds to requests for the
// management AuthConfig list and counts them.
func authConfigNameServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/authconfigs", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"github-eu"},"type":"githubConfig","displayName":"GitHub EU","clientSecret":"cattle-global-data:github-clientsecret"},
			{"metadata":{"name":"local"},"type":"localConfig"},
			{"metadata":{"name":"okta"},"type":"oktaConfig","displayName":"Okta"}
		]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestAuthConfigPositionalSiteFullCallback drives the real completion callback
// for the authconfig name: with the network gate on, it offers the names with
// their display names (local has none). With the gate off, it offers no
// candidates. For patch it offers no candidates, because the row refuses patch;
// there is no delete command to complete. Every offered name is one the server
// listed. NOT parallel: it sets the process environment.
func TestAuthConfigPositionalSiteFullCallback(t *testing.T) {
	srv, hits := authConfigNameServer(t)

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

	run := func(t *testing.T, words []string) string {
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

	t.Run("get authconfig offers the names with display names", func(t *testing.T) {
		setup(t, true)
		got := run(t, []string{"get", "authconfig"})
		assert.Equal(t, "github-eu:GitHub EU\nlocal\nokta:Okta\n", got)
		assert.NotContains(t, got, "cattle-global-data")
	})

	t.Run("network gate off offers no candidates", func(t *testing.T) {
		setup(t, false)
		before := hits.Load()
		assert.Empty(t, run(t, []string{"get", "authconfig"}))
		assert.Equal(t, before, hits.Load(), "no request is made")
	})

	t.Run("patch authconfig offers no candidates", func(t *testing.T) {
		setup(t, true)
		assert.Empty(t, run(t, []string{"patch", "authconfig"}))
	})

	t.Run("there is no delete authconfig to complete", func(t *testing.T) {
		setup(t, true)
		assert.Empty(t, run(t, []string{"delete", "authconfig"}))
	})
}
