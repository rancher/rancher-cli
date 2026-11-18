package bykeycluster

import "github.com/rancher/rancher-cli/internal/engine/kinds"

type getter interface{ Get(name string) }

func read(c getter) {
	_, _ = kinds.ByKey("managementcluster")
	c.Get("c-abc")
}
