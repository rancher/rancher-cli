// A management clusters GroupVersionResource literal.
package gvrliteral

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func Read(ctx context.Context, c engine.ClusterClient) {
	_, _ = c.Get(ctx, schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusters"}, "", "c-1")
}
