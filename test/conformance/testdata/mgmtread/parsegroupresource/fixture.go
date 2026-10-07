// A clusters GroupVersionResource from ParseGroupResource and WithVersion.
package parsegroupresource

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func Read(ctx context.Context, c engine.ClusterClient) {
	_, _ = c.Get(ctx, schema.ParseGroupResource("clusters.management.cattle.io").WithVersion("v3"), "", "c-1")
}
