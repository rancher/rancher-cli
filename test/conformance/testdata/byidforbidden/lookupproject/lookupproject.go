package lookupproject

import "github.com/rancher/rancher-cli/internal/engine/kinds"

type getter interface{ Get(name string) }

func read(c getter) {
	_, _ = kinds.Lookup("project")
	c.Get("p-abc")
}
