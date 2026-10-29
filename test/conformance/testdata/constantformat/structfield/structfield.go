package structfield

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

var hintSet = struct{ Access string }{clierr.Rancher + " access clusters"}

func hint(x string) string { return fmt.Sprintf(hintSet.Access+" --for %s", x) }
