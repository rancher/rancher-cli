package unknownfunction

import "github.com/rancher/rancher-cli/internal/clierr"

func build() *clierr.Error { return clierr.New(clierr.CodeNotFound, "not found") }

func attach(c []clierr.Candidate) *clierr.Error {
	return build().WithCandidates(c)
}
