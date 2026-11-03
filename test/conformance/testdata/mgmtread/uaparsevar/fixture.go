// A ParseGroupResource of userattributes held in a variable before WithVersion.
package uaparsevar

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func Read(ctx context.Context, local engine.ClusterClient, userID string) {
	gr := schema.ParseGroupResource("userattributes.management.cattle.io")
	_, _ = local.Get(ctx, gr.WithVersion("v3"), "", userID)
}
