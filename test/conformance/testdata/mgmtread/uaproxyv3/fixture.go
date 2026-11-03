// A UserAttribute read by a constant path through the local proxy.
package uaproxyv3

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
)

func Read(ctx context.Context, c engine.RootClient, id string) {
	_, _ = c.GetJSON(ctx, "/k8s/clusters/local/apis/management.cattle.io/v3/userattributes/"+id)
}
