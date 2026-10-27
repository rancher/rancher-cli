package shortvar

import "github.com/rancher/rancher-cli/internal/clierr"

func build() clierr.Candidate {
	command := clierr.MarkRemedy("rancher get cluster")
	named := command + " --help"
	return clierr.Candidate{ID: "c-1", Name: named}
}
