package functionvaluecall

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

var textFn = extraText

func extraText() string { return clierr.Rancher + " access clusters" }

func hint(x string) string { return fmt.Sprintf(textFn()+" --for %s", x) }
