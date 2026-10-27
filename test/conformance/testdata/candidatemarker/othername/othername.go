package othername

import "github.com/rancher/rancher-cli/internal/clierr"

const Rancher = "a constant of this package"

func MarkRemedy(s string) string { return s }

func build() clierr.Candidate {
	return clierr.Candidate{ID: MarkRemedy("c-1"), Name: Rancher}
}
