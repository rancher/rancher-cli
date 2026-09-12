// A raw socket beside the seam.
package complete

import (
	"net"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

var _ = []any{net.Dial, completion.NewConnection}
