// A local management GroupVersion with a clusters resource.
package gvlocal

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func Read(ctx context.Context, c engine.ClusterClient) {
	gv := schema.GroupVersion{Group: "management.cattle.io", Version: "v3"}
	_, _ = c.List(ctx, gv.WithResource("clusters"), "", "")
}
