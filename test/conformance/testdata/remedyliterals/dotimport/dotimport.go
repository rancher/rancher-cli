package dotimport

import (
	"fmt"

	. "github.com/rancher/rancher-cli/internal/clierr"
)

func commands() []string {
	return []string{
		Rancher + "get users",
		Rancher + " get users",
		fmt.Sprintf("%sget roletemplates", Rancher),
	}
}
