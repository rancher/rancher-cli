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

// nodeDriverServer serves the management NodeDriver list, counting hits. One
// name carries shell metacharacters to exercise the AC-21 sanitize boundary.
func nodeDriverServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/k8s/clusters/local/apis/management.cattle.io/v3/nodedrivers", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"items":[
			{"metadata":{"name":"amazonec2"},"spec":{"displayName":"Amazon EC2","active":true,"builtin":true}},
			{"metadata":{"name":"digitalocean"},"spec":{"displayName":"DigitalOcean","active":false,"builtin":true}},
			{"metadata":{"name":"evil;rm -rf"},"spec":{"displayName":"hostile","active":true,"builtin":false}}
		]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestNodeDriverPositionalSiteFullCallback drives the real adaptEntry
// callback through BuildRoot/root.Run under the v2 protocol, mirroring
// TestSubjectPositionalSiteFullCallback: network on → live NodeDriver names;
// network off → empty.
// NOT parallel: t.Setenv mutates env globals, setStoredConfig mutates storedConfig.
func TestNodeDriverPositionalSiteFullCallback(t *testing.T) {
	srv, _ := nodeDriverServer(t)

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
		origArgv, origWord, origProto, origV1 := rawArgv, rawCurrentWord, rawProtoV2, rawV1Completion
		t.Cleanup(func() {
			rawArgv = origArgv
			rawCurrentWord = origWord
			rawProtoV2 = origProto
			rawV1Completion = origV1
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

	t.Run("get nodedriver positional → driver names", func(t *testing.T) {
		setup(t, true)
		got := run(t, []string{"get", "nodedriver"}, "")
		assert.Contains(t, got, "amazonec2")
		assert.Contains(t, got, "digitalocean")
		assert.NotContains(t, got, "evil")
	})

	t.Run("network gate off → empty", func(t *testing.T) {
		setup(t, false)
		got := run(t, []string{"get", "nodedriver"}, "")
		assert.Empty(t, got)
	})
}
