// The cluster kind's GVR held in a variable, and in a second one.
package gvrvar

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
)

var clusterKind, _ = kinds.ByKey("managementcluster")

func Read(ctx context.Context, c engine.ClusterClient) {
	g := clusterKind.GVR
	_, _ = c.Get(ctx, g, "", "c-1")
}

func ReadThroughAnAlias(ctx context.Context, c engine.ClusterClient) {
	g := clusterKind.GVR
	g2 := g
	_, _ = c.List(ctx, g2, "", "")
}
