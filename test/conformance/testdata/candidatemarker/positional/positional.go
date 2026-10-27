package positional

import "github.com/rancher/rancher-cli/internal/clierr"

func build() []clierr.Candidate {
	return []clierr.Candidate{{"c-1", clierr.Rancher}}
}
