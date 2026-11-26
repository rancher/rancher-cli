package ambiguousfull

import "github.com/rancher/rancher-cli/internal/clierr"

func attach(c []clierr.Candidate) *clierr.Error {
	return clierr.New(clierr.CodeAmbiguousSelection, "ambiguous").WithHint("pass the id").WithCandidates(c)
}
