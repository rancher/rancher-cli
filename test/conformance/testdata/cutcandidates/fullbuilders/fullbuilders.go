package fullbuilders

import "github.com/rancher/rancher-cli/internal/clierr"

func attach(c []clierr.Candidate) *clierr.Error {
	return clierr.New(clierr.CodeNotFound, "not found").WithCandidates(c)
}

func fromNames(names []string) []clierr.Candidate {
	out := make([]clierr.Candidate, 0, len(names))
	for _, n := range names {
		out = append(out, clierr.Candidate{ID: n, Name: n})
	}
	return out
}
