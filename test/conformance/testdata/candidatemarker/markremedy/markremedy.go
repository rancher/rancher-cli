package markremedy

import "github.com/rancher/rancher-cli/internal/clierr"

func build(cmd string) clierr.Candidate {
	return clierr.Candidate{ID: clierr.MarkRemedy(cmd), Name: "x"}
}
