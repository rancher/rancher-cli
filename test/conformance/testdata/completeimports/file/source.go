// A file read beside the seam.
package complete

import (
	"io/fs"
	"os"

	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

var _ = []any{fs.ReadFile, os.ReadFile, completion.NewConnection}
