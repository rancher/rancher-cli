// The UserAttribute GroupVersionResource as a literal passed to Get.
package uagvrliteral

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func Read(ctx context.Context, local engine.ClusterClient, userID string) {
	_, _ = local.Get(ctx, schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "userattributes"}, "", userID)
}
