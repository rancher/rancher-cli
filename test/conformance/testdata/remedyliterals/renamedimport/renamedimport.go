package renamedimport

import (
	"fmt"

	ce "github.com/rancher/rancher-cli/internal/clierr"
)

func commands() []string {
	return []string{
		ce.Rancher + "get users",
		ce.Rancher + " get users",
		fmt.Sprintf("%sget roletemplates", ce.Rancher),
		fmt.Sprintf("enable with: %s", ce.Rancher),
	}
}
