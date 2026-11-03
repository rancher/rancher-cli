// A management GroupVersion's WithResource of userattributes, listed.
package uawithresource

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func Read(ctx context.Context, local engine.ClusterClient) {
	_, _ = local.List(ctx, schema.GroupVersion{Group: "management.cattle.io", Version: "v3"}.WithResource("userattributes"), "", "")
}
