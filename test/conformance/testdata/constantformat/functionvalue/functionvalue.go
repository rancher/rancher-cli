package functionvalue

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

var textFn = extraText

var extra = textFn()

func extraText() string { return clierr.Rancher + " access clusters" }

func hint(x string) string { return fmt.Sprintf(extra+" --for %s", x) }
