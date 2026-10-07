// Reads the scan allows: the steve collection, a provisioning cluster, a
// project get, a joined steve path and another management resource.
package accepted

import (
	"context"
	"net/http"
	"path"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func Read(ctx context.Context, c engine.ClusterClient) {
	_, _ = http.NewRequestWithContext(ctx, http.MethodGet, "/v1/management.cattle.io.clusters", nil)
	_, _ = http.NewRequestWithContext(ctx, http.MethodGet, path.Join("/v1", "management.cattle.io.clusters"), nil)
	prov, _ := kinds.LookupGVK("provisioning.cattle.io/v1", "Cluster")
	_, _ = c.Get(ctx, prov.GVR, "fleet-default", "x")
	project, _ := kinds.ByKey("project")
	_, _ = c.Get(ctx, project.GVR, "c-1", "p-1")
	_, _ = c.List(ctx, schema.GroupVersion{Group: "management.cattle.io", Version: "v3"}.WithResource("users"), "", "")
}
