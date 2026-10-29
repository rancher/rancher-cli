package localvariable

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func hint(x string) string {
	m := clierr.Rancher
	return fmt.Sprintf(m+" access clusters --for %s", x)
}

func plain(x string) string {
	local := "listed by: "
	return fmt.Sprintf(local+"%s", x)
}
