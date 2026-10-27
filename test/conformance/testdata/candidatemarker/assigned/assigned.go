package assigned

import "github.com/rancher/rancher-cli/internal/clierr"

func build(c *clierr.Candidate) {
	c.Name = clierr.Rancher + " get cluster"
}
