package slicevariable

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

var hintParts = []string{clierr.Rancher + " access clusters"}

func hint(x string) string { return fmt.Sprintf(hintParts[0]+" --for %s", x) }
