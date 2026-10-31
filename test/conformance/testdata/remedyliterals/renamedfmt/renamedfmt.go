package renamedfmt

import (
	f "fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func command(n string) []string {
	return []string{
		f.Sprintf("%sget users", clierr.Rancher),
		f.Sprintf("%s get users", clierr.Rancher),
		f.Sprint("rancher", n),
		f.Sprint(clierr.Rancher, "get", " roletemplates"),
	}
}
