package notfoundalias

import "github.com/rancher/rancher-cli/internal/clierr"

const code = clierr.CodeNotFound

func attach(c []clierr.Candidate) *clierr.Error {
	return clierr.New(code, "not found").WithCandidates(c)
}
