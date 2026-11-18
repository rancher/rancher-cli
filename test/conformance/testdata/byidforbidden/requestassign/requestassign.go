package requestassign

import "github.com/rancher/rancher-cli/internal/engine/steve"

func read(typ, v string) steve.Request {
	r := steve.Request{Type: typ}
	r.Name = v
	return r
}
