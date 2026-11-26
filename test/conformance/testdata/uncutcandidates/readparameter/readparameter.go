package readparameter

import "github.com/rancher/rancher-cli/internal/clierr"

func pass(from *clierr.Error) *clierr.Error {
	return rebuild(from.Candidates)
}

func rebuild(c []clierr.Candidate) *clierr.Error {
	return clierr.New(clierr.CodeRBAC, "refused").WithCandidates(c)
}
