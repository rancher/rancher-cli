package keyed

import "github.com/rancher/rancher-cli/internal/clierr"

func build() clierr.Candidate {
	return clierr.Candidate{ID: "c-1", Name: clierr.Rancher + " get cluster"}
}
