package localclosure

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func hint(x string) string {
	g := func() string { return clierr.Rancher + " access clusters" }
	return fmt.Sprintf(g()+" --for %s", x)
}
