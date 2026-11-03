// A GroupResource literal of userattributes given a version, then read.
package uagrliteral

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func Read(ctx context.Context, local engine.ClusterClient, userID string) {
	_, _ = local.Get(ctx, schema.GroupResource{Group: "management.cattle.io", Resource: "userattributes"}.WithVersion("v3"), "", userID)
}
