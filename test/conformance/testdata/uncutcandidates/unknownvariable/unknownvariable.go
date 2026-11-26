package unknownvariable

import "github.com/rancher/rancher-cli/internal/clierr"

func attach(e *clierr.Error, c []clierr.Candidate) *clierr.Error {
	return e.WithCandidates(c)
}
