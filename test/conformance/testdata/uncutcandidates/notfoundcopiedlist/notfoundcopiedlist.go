package notfoundcopiedlist

import "github.com/rancher/rancher-cli/internal/clierr"

func pass(from *clierr.Error) *clierr.Error {
	return clierr.New(clierr.CodeNotFound, "not found").WithCandidates(from.Candidates)
}
