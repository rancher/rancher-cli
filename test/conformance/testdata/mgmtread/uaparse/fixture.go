// A ParseGroupResource of userattributes given a version, then read.
package uaparse

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func Read(ctx context.Context, local engine.ClusterClient, userID string) {
	_, _ = local.Get(ctx, schema.ParseGroupResource("userattributes.management.cattle.io").WithVersion("v3"), "", userID)
}
