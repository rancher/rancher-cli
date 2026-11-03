// A UserAttribute path built by fmt.Sprintf with a value that is not constant.
package uasprintf

import (
	"context"
	"fmt"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
)

func Read(ctx context.Context, c engine.RootClient, id string) {
	_, _ = c.GetJSON(ctx, fmt.Sprintf("/k8s/clusters/local/v1/%s/%s", steve.UserAttributesType, id))
}
