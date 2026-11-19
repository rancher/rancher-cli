package namedresult

import "github.com/rancher/rancher-cli/internal/clierr"

func request() (q clierr.Request) {
	q.Server = "s"
	return
}
