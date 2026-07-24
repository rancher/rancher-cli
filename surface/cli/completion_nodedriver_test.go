package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
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

// TestLivePositionalNodeDriverCandidates pins the management branch case for
// the nodedriver kind: `get nodedriver <TAB>` lists v3 NodeDrivers from the
// local (management) cluster — no downstream cluster needed — and hostile
// names are dropped by the sanitize charset.
func TestLivePositionalNodeDriverCandidates(t *testing.T) {
	t.Parallel()
	srv, hits := nodeDriverServer(t)
	cap := TokenCapability{Token: "token-abc:secret", ServerURL: srv.URL}
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get nodedriver"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
	assert.Equal(t, []string{"amazonec2", "digitalocean"}, got)
	assert.Equal(t, int32(1), hits.Load(), "exactly one management list request")
}

// TestLivePositionalNodeDriverTokenBoundary pins the management-token
// boundary: a missing token or an exec-cred-only (IDOnly) capability cannot
// authorize the management cluster list — nil, with ZERO requests.
func TestLivePositionalNodeDriverTokenBoundary(t *testing.T) {
	t.Parallel()
	srv, hits := nodeDriverServer(t)
	site := CompletionSite{Kind: SitePositional, Index: 0}
	d := runtime.Descriptor{Name: "get nodedriver"}
	for _, cap := range []TokenCapability{
		{ServerURL: srv.URL},
		{Token: "downstream", IDOnly: true, ServerURL: srv.URL},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		got := livePositionalCandidates(ctx, srv.Client(), site, d, cap, "", "", nil)
		cancel()
		assert.Nil(t, got)
	}
	assert.Zero(t, hits.Load(), "token boundary must return nil WITHOUT a request")
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
			CurrentContext: "ctx",
			Contexts:       map[string]config.Context{"ctx": {Server: "srv"}},
			Servers:        map[string]config.Server{"srv": {URL: srv.URL}},
			Completion:     config.CompletionConfig{Network: networkOn},
		}
		cfg.SetPath(config.FilePath(dir))
		require.NoError(t, cfg.Write())
		setStoredConfig(cfg, dir)
		t.Setenv(config.EnvConfigDir, dir)
		t.Setenv(config.EnvToken, "mgmt-token")
		t.Setenv("RANCHER_CLI_CREDENTIAL_STORE", "file")
	}

	run := func(t *testing.T, words []string, currentWord string) string {
		t.Helper()
		origArgv, origWord, origProto := rawArgv, rawCurrentWord, rawProtoV2
		t.Cleanup(func() { rawArgv = origArgv; rawCurrentWord = origWord; rawProtoV2 = origProto })
		scriptArgs := append([]string{"--rancher-completion-proto=2"}, words...)
		scriptArgs = append(scriptArgs, "--rancher-completion-cur="+currentWord, "--generate-shell-completion")
		CaptureArgv(scriptArgs)
		root, err := BuildRoot("test")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer = &out
		_ = root.Run(context.Background(), SanitizedArgs(append([]string{"rancher"}, scriptArgs...)))
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
