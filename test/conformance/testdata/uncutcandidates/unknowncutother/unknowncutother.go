package unknowncutother

import "github.com/rancher/rancher-cli/internal/clierr"

func attach(e *clierr.Error, c []clierr.Candidate, n int) *clierr.Error {
	return e.WithCandidatesCut(c, n)
}
