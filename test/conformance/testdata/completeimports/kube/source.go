// The kube client beside the seam.
package complete

import (
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

var _ = []any{kube.HTTPClient, completion.NewConnection}
