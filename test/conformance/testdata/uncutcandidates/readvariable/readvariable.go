package readvariable

import "github.com/rancher/rancher-cli/internal/clierr"

func count(from *clierr.Error) int {
	c := from.Candidates
	return len(c)
}
