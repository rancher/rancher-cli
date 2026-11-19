package arraytype

import "github.com/rancher/rancher-cli/internal/clierr"

func requests() int {
	var q [2]clierr.Request
	q[0].Server = "s"
	return len(q)
}
