package lookupclusteralias

import "github.com/rancher/rancher-cli/internal/engine/kinds"

type getter interface{ Get(name string) }

func read(c getter) {
	_, _ = kinds.Lookup("cluster")
	c.Get("c-abc")
}
