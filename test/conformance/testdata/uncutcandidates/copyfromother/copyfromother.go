package copyfromother

import "github.com/rancher/rancher-cli/internal/clierr"

type holder struct{ Candidates []clierr.Candidate }

func pass(h holder) *clierr.Error {
	return clierr.New(clierr.CodeAmbiguousSelection, "ambiguous").WithCandidates(h.Candidates)
}
