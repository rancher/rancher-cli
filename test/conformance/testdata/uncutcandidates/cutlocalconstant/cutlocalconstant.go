package cutlocalconstant

import "github.com/rancher/rancher-cli/internal/clierr"

const limit = 10

func attach(c []clierr.Candidate) *clierr.Error {
	return clierr.New(clierr.CodeNotFound, "not found").WithCandidatesCut(c, limit)
}
