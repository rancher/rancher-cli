// A UserAttribute GroupResource held in a variable before WithVersion.
package uagrvar

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func Read(ctx context.Context, local engine.ClusterClient, userID string) {
	gr := schema.GroupResource{Group: "management.cattle.io", Resource: "userattributes"}
	_, _ = local.Get(ctx, gr.WithVersion("v3"), "", userID)
}
