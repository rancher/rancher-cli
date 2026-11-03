// A steve request for UserAttributes on cluster local, handed to a getter.
package uasteverequest

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
)

func Read(ctx context.Context, get steve.Getter, id string) {
	_, _ = get(ctx, steve.Request{Cluster: engine.LocalCluster, Type: "management.cattle.io.userattributes", Name: id})
}
