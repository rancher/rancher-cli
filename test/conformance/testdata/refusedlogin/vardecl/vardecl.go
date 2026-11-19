package vardecl

import "github.com/rancher/rancher-cli/internal/clierr"

func request() *clierr.Request {
	var q clierr.Request
	q.Server = "s"
	return &q
}
