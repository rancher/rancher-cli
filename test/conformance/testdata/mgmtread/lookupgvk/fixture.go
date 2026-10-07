// The management Cluster kind looked up by group/version and kind.
package lookupgvk

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
)

func Read(ctx context.Context, c engine.ClusterClient) {
	k, _ := kinds.LookupGVK("management.cattle.io/v3", "Cluster")
	_, _ = c.Get(ctx, k.GVR, "", "c-1")
}
