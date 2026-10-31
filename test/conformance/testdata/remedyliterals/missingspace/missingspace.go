package missingspace

import (
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func command(name string) []string {
	return []string{
		clierr.Rancher + "get users",
		clierr.Rancher + " get users",
		clierr.Rancher + fmt.Sprintf(" get %s", name),
		clierr.Rancher + fmt.Sprintf("get %s", name),
	}
}
