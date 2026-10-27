package markerconstant

import "github.com/rancher/rancher-cli/internal/clierr"

func build(name string) clierr.Candidate {
	return clierr.Candidate{ID: "c-1", Name: name + clierr.RemedyMarker}
}
