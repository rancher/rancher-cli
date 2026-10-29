package markervariable

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

var extra = clierr.RemedyMarker + "rancher access clusters"

func command(x string) string { return fmt.Sprintf(extra+" --for %s", x) }
