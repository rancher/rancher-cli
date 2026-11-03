// A UserAttribute steve request given the local cluster after it is built.
package uarequestassign

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
)

func Read(ctx context.Context, get steve.Getter, id string) {
	r := steve.Request{Type: steve.UserAttributesType, Name: id}
	r.Cluster = engine.LocalCluster
	_, _ = get(ctx, r)
}
