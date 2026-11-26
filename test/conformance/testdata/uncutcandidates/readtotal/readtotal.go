package readtotal

import "github.com/rancher/rancher-cli/internal/clierr"

func total(from *clierr.Error) int {
	return from.CandidatesTotal
}
