// A process execution beside the seam.
package complete

import (
	"os/exec"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

var _ = []any{exec.Command, completion.NewConnection}
