// An environment read beside the seam.
package complete

import (
	"os"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

var _ = []any{os.Getenv, completion.NewConnection}
