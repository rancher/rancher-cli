package notfoundmatchesfirst

import "github.com/rancher/rancher-cli/internal/clierr"

func attach(c []clierr.Candidate, typed string) *clierr.Error {
	return clierr.New(clierr.CodeNotFound, "not found").WithCandidatesMatchesFirst(c, typed)
}
