// A package-level management GroupVersion with a clusters resource.
package gvpackage

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var mgmtGV = schema.GroupVersion{Group: "management.cattle.io", Version: "v3"}

func Read(ctx context.Context, c engine.ClusterClient) {
	_, _ = c.Get(ctx, mgmtGV.WithResource("clusters"), "", "c-1")
}
