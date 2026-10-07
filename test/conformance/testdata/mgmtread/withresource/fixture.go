// A GroupVersion literal of the management group with a clusters or a
// projects resource.
package withresource

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func Read(ctx context.Context, c engine.ClusterClient) {
	_, _ = c.Get(ctx, schema.GroupVersion{Group: "management.cattle.io", Version: "v3"}.WithResource("clusters"), "", "c-1")
	_, _ = c.List(ctx, schema.GroupVersion{Group: "management.cattle.io", Version: "v3"}.WithResource("projects"), "c-1", "")
}
