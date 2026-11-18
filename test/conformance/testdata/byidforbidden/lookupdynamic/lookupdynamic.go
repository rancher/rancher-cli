package lookupdynamic

import "github.com/rancher/rancher-cli/internal/engine/kinds"

type getter interface{ Get(name string) }

func read(c getter, q string) {
	_, _ = kinds.Lookup(q)
	c.Get("x")
}
