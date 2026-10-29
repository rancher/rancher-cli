package directcall

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func extraText() string { return clierr.Rancher + " access clusters" }

func hint(x string) string { return fmt.Sprintf(extraText()+" --for %s", x) }
