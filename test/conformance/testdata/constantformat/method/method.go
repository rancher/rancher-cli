package method

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

type hints struct{}

func (hints) text() string { return clierr.Rancher + " access clusters" }

var extra = hints{}.text()

func hint(x string) string { return fmt.Sprintf(extra+" --for %s", x) }
