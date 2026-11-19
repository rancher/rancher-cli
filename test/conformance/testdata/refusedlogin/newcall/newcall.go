package newcall

import "github.com/rancher/rancher-cli/internal/clierr"

func request() *clierr.Request {
	q := new(clierr.Request)
	q.Server = "s"
	return q
}
