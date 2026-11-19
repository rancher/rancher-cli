package makeslice

import "github.com/rancher/rancher-cli/internal/clierr"

func requests() []clierr.Request {
	q := make([]clierr.Request, 2)
	q[0].Server = "s"
	return q
}
