// Steve's UserAttribute type behind the local proxy, by a constant path.
package uaproxyv1

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
)

func Read(ctx context.Context, c engine.RootClient) {
	_, _ = c.GetJSON(ctx, "/k8s/clusters/local/v1/management.cattle.io.userattributes")
}
