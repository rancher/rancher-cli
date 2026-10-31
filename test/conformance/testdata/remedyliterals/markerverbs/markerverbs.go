package markerverbs

import (
	"fmt"
	"io"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func command(w io.Writer) {
	_ = fmt.Sprintf("%s%s", clierr.Rancher, " get users")
	fmt.Fprintf(w, "%s\n", clierr.Rancher)
	_ = fmt.Sprintf("%s%%x", clierr.Rancher)
}
