// The credential store beside the seam.
package complete

import (
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

var _ = []any{config.Load, completion.NewConnection}
