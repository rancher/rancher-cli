package declaredvar

import "github.com/rancher/rancher-cli/internal/clierr"

func build() clierr.Candidate {
	var command = clierr.Rancher + " get cluster"
	var c clierr.Candidate
	c.ID = command
	return c
}
