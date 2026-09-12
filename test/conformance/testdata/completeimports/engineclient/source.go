// The engine's client ports beside the seam.
package complete

import (
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

var _ = []any{engine.LocalCluster, completion.NewConnection}
