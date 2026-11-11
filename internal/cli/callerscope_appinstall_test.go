package cli

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/rancher/rancher-cli/internal/testrancher"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAppInstallReadsSettingsAndProjectsThroughSteve asserts an app install run
// as a project member, who may not get cluster local, reads the Settings
// and the projects it adds values from through steve on the Rancher
// server, and sends no request through the local cluster's Kubernetes API.
// The chart declares a registry path, so the system-default-registry
// Setting is read too.
func TestAppInstallReadsSettingsAndProjectsThroughSteve(t *testing.T) {
	h := newCallerHarnessWith(t, false, func(o *testrancher.Options) {
		next := o.Fallthrough
		o.Fallthrough = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			write := func(status int, v any) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(v)
			}
			switch {
			case r.URL.Path == "/k8s/clusters/c-abc/apis/catalog.cattle.io/v1/clusterrepos/r":
				write(http.StatusOK, map[string]any{"apiVersion": "catalog.cattle.io/v1", "kind": "ClusterRepo",
					"metadata": map[string]any{"name": "r", "resourceVersion": "1"}, "spec": map[string]any{"url": "https://charts.example.test"}})
			case r.URL.Path == "/k8s/clusters/c-abc/v1/catalog.cattle.io.clusterrepos/r" && r.URL.Query().Get("link") == "index":
				write(http.StatusOK, map[string]any{"entries": map[string]any{"c": []any{map[string]any{"version": "1.0.0", "digest": "sha256:a"}}}})
			case r.URL.Path == "/k8s/clusters/c-abc/v1/catalog.cattle.io.clusterrepos/r" && r.URL.Query().Get("link") == "info":
				write(http.StatusOK, map[string]any{"values": map[string]any{"global": map[string]any{"cattle": map[string]any{"systemDefaultRegistry": ""}}},
					"chart": map[string]any{"annotations": map[string]any{}}})
			case strings.HasPrefix(r.URL.Path, "/k8s/clusters/c-abc/apis/catalog.cattle.io/v1/namespaces/default/apps/"):
				write(http.StatusNotFound, map[string]any{"kind": "Status", "apiVersion": "v1", "metadata": map[string]any{},
					"status": "Failure", "message": "not found", "reason": "NotFound", "code": 404})
			default:
				next.ServeHTTP(w, r)
			}
		})
	})
	run := h.run(t, strings.Fields("app install web --cluster prod --namespace default --repo r --chart c --dry-run -o json"))
	require.Nil(t, run.err, "stdout %s stderr %s", run.stdout, run.stderr)

	var paths []string
	for _, q := range run.requests {
		paths = append(paths, q.path)
		assert.False(t, strings.HasPrefix(q.path, "/k8s/clusters/local/"), "a request through the local cluster: %s", q)
	}
	for _, want := range []string{
		"/v1/management.cattle.io.settings/server-url",
		"/v1/management.cattle.io.settings/system-default-registry",
		testrancher.ProjectsCollectionPath + "/c-abc",
		"/v1/management.cattle.io.clusters/c-abc",
	} {
		assert.True(t, slices.Contains(paths, want), "no request for %s in %v", want, paths)
	}
}
