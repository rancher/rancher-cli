package namedresult

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

var extra = extraText()

func extraText() (s string) {
	s = clierr.Rancher + " access clusters"
	return
}

func hint(x string) string { return fmt.Sprintf(extra+" --for %s", x) }
