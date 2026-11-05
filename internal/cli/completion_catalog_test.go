package cli_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/cli"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFullCallbackCatalogNames asserts, through the real completion callback,
// that for get the names of the catalog kinds are fetched live from the target
// cluster: an app under --namespace, a clusterrepo with no namespace, and no
// names once --project is typed; and that the always-refused patch of each
// offers no names. Not parallel: it sets the process environment
// and the captured command line.
func TestFullCallbackCatalogNames(t *testing.T) {
	clusterItems := `{"count":1,"revision":"1","data":[{"id":"c-abc","metadata":{"name":"c-abc"},"spec":{"displayName":"prod"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/catalog.cattle.io/v1/namespaces/cattle-system/apps"):
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"rancher-webhook"}}]}`))
		case strings.HasSuffix(path, "/catalog.cattle.io/v1/clusterrepos"):
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"rancher-charts"}}]}`))
		case strings.HasSuffix(path, "/catalog.cattle.io/v1/namespaces/cattle-system/operations"):
			_, _ = w.Write([]byte(`{"items":[{"metadata":{"name":"helm-operation-x"}}]}`))
		default:
			_, _ = w.Write([]byte(clusterItems))
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cfg := &config.Config{
		CurrentServer: "srv",
		Servers:       map[string]config.Server{"srv": {URL: srv.URL}},
		Completion:    config.CompletionConfig{Network: true},
	}
	cfg.SetPath(config.FilePath(dir))
	require.NoError(t, cfg.Write())
	t.Setenv(config.EnvConfigDir, dir)
	t.Setenv(config.EnvToken, "tok")
	cli.SetStoredConfigForTest(t, cfg, dir)

	complete := func(t *testing.T, words ...string) string {
		t.Helper()
		script := append([]string{"--rancher-completion-proto=2"}, words...)
		script = append(script, "--rancher-completion-cur=", "--generate-shell-completion")
		cli.CaptureArgvForTest(script)
		root, err := cli.BuildRoot("test")
		require.NoError(t, err)
		var out bytes.Buffer
		root.Writer = &out
		root.ErrWriter = io.Discard
		_ = root.Run(t.Context(), cli.SanitizedArgsForTest(append([]string{"rancher"}, script...)))
		return out.String()
	}

	assert.Contains(t, complete(t, "get", "app", "--cluster", "c-abc", "--namespace", "cattle-system"), "rancher-webhook")
	assert.Contains(t, complete(t, "get", "operation", "--cluster", "c-abc", "--namespace", "cattle-system"), "helm-operation-x")
	assert.Contains(t, complete(t, "get", "clusterrepo", "--cluster", "c-abc"), "rancher-charts")

	// The fake returns the names of the typed namespace, so a completer that
	// offered names under a project would show them here.
	assert.NotContains(t, complete(t, "get", "app", "--cluster", "c-abc", "--namespace", "cattle-system", "--project", "p-abc"), "rancher-webhook",
		"a typed project offers no names")
	assert.NotContains(t, complete(t, "get", "operation", "--cluster", "c-abc", "--namespace", "cattle-system", "--project", "p-abc"), "helm-operation-x",
		"a typed project offers no names")

	// A patch of these kinds is always refused, so it offers no names.
	assert.NotContains(t, complete(t, "patch", "app", "--cluster", "c-abc", "--namespace", "cattle-system"), "rancher-webhook")
	assert.NotContains(t, complete(t, "patch", "operation", "--cluster", "c-abc", "--namespace", "cattle-system"), "helm-operation-x")
	assert.NotContains(t, complete(t, "patch", "clusterrepo", "--cluster", "c-abc"), "rancher-charts")
}
