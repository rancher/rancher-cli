package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const uiPluginBase = "/k8s/clusters/local/apis/catalog.cattle.io/v1/namespaces/"

// uiPluginServer serves one UIPlugin in the extension namespace and one in
// another namespace, and records the paths it is asked for.
type uiPluginServer struct {
	mu    sync.Mutex
	paths []string
}

func (s *uiPluginServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.Path)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	item := func(name, namespace string) map[string]any {
		return map[string]any{
			"apiVersion": "catalog.cattle.io/v1", "kind": "UIPlugin",
			"metadata": map[string]any{"name": name, "namespace": namespace},
			"spec":     map[string]any{"plugin": map[string]any{"name": name, "version": "1.0.0"}},
			"status":   map[string]any{"cacheState": "cached"},
		}
	}
	list := func(items ...any) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "catalog.cattle.io/v1", "kind": "UIPluginList",
			"metadata": map[string]any{"resourceVersion": "1"}, "items": items,
		})
	}
	switch r.URL.Path {
	case uiPluginBase + "cattle-ui-plugin-system/uiplugins":
		list(item("in-extension-namespace", "cattle-ui-plugin-system"))
	case uiPluginBase + "other/uiplugins":
		list(item("in-other-namespace", "other"))
	default:
		http.NotFound(w, r)
	}
}

// TestGetUIPluginsReadsTheExtensionNamespaceByDefault asserts that get
// uiplugins without --namespace lists the namespace Rancher's extension chart
// installs into, and only that one, and that --namespace lists another.
func TestGetUIPluginsReadsTheExtensionNamespaceByDefault(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"get", "uiplugins", "-o", "json"}, "in-extension-namespace"},
		{[]string{"get", "uiplugins", "--namespace", "other", "-o", "json"}, "in-other-namespace"},
	} {
		restoreRawArgv(t)
		extraArgsEnv(t)
		s := &uiPluginServer{}
		srv := httptest.NewTLSServer(s)
		t.Setenv(config.EnvURL, srv.URL)
		t.Setenv(config.EnvCACert, string(pemFromServer(t, srv)))
		stdout, stderr, code := runBounded(t, newRoot(t), tc.args)
		srv.Close()
		require.Zero(t, code, stderr)
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(stdout), &env), stdout)
		var names []string
		var walk func(v any)
		walk = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				if meta, ok := x["metadata"].(map[string]any); ok {
					if name, ok := meta["name"].(string); ok && strings.HasPrefix(name, "in-") {
						names = append(names, name)
					}
				}
				for _, child := range x {
					walk(child)
				}
			case []any:
				for _, child := range x {
					walk(child)
				}
			}
		}
		var decoded any
		require.NoError(t, json.Unmarshal(env.Data, &decoded))
		walk(decoded)
		assert.Equal(t, []string{tc.want}, names, "the listing holds the one UIPlugin of the namespace read")
	}
}
