package mapvalue

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

var hintsByName = map[string]string{"access": clierr.Rancher + " access clusters"}

func hint(x string) string { return fmt.Sprintf(hintsByName["access"]+" --for %s", x) }
