//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
)

// discoveryResource is one entry of a kube-apiserver APIResourceList.
type discoveryResource struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Namespaced bool     `json:"namespaced"`
	Verbs      []string `json:"verbs"`
}

// TestServerTableRowsAreServedByKubeAPIServer asserts that every kind row
// marked ServerTable is a resource the kube-apiserver serves at the row's
// group and version. The discovery list of that group and version must have
// an entry whose name is the row's resource and whose kind is the row's
// K8sKind. The entry must be namespaced exactly when the row's policy is
// NSPlain. Its verbs must include get, list and patch, and also delete when
// the row is deletable.
//
// The test sends its requests to newRouter, the e2e suite's handler that takes
// the place of Rancher's cluster proxy. newRouter forwards them to the test
// kube-apiserver. The test shows that Kubernetes itself serves the row's
// group, version, resource and namespaced setting. It shows nothing about what
// Rancher's own proxy returns. A ServerTable row for a resource that a CRD or
// an aggregated API serves fails this test, because the test kube-apiserver
// does not serve it. Such a resource may not support Tables. Its row needs a
// fidelity probe of its own.
func TestServerTableRowsAreServedByKubeAPIServer(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(newRouter(staticIdentityResolver(shimIdentities)))
	t.Cleanup(srv.Close)

	rows := 0
	for _, k := range kinds.All() {
		if !k.ServerTable {
			continue
		}
		rows++
		t.Run(k.Key, func(t *testing.T) {
			t.Parallel()
			path := "/k8s/clusters/c-test/api/" + k.GVR.Version
			if k.GVR.Group != "" {
				path = "/k8s/clusters/c-test/apis/" + k.GVR.Group + "/" + k.GVR.Version
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+testToken)
			resp, err := srv.Client().Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			require.Equal(t, http.StatusOK, resp.StatusCode, "kind %q: the kube-apiserver serves no discovery list for %q", k.Key, path)

			var list struct {
				Resources []discoveryResource `json:"resources"`
			}
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))

			i := slices.IndexFunc(list.Resources, func(r discoveryResource) bool {
				return !strings.Contains(r.Name, "/") && r.Name == k.GVR.Resource
			})
			require.GreaterOrEqual(t, i, 0, "kind %q: %q lists no resource %q", k.Key, path, k.GVR.Resource)
			got := list.Resources[i]
			assert.Equal(t, k.K8sKind, got.Kind, "kind %q: the kind of resource %q differs from the row's K8sKind", k.Key, k.GVR.Resource)

			switch k.Policy {
			case kinds.NSPlain:
				assert.True(t, got.Namespaced, "kind %q: row is NSPlain but the resource is cluster-scoped", k.Key)
			case kinds.NSNone:
				assert.False(t, got.Namespaced, "kind %q: row is NSNone but the resource is namespaced", k.Key)
			default:
				assert.Fail(t, "ServerTable kind has a namespace policy other than NSPlain and NSNone", "kind %q has policy %q", k.Key, k.Policy)
			}

			verbs := []string{"get", "list", "patch"}
			if k.Deletable {
				verbs = append(verbs, "delete")
			}
			for _, verb := range verbs {
				assert.Contains(t, got.Verbs, verb, "kind %q: the kube-apiserver does not serve verb %q for %q", k.Key, verb, k.GVR.Resource)
			}
		})
	}
	assert.Positive(t, rows, "the kind table has ServerTable rows")
}
