package othercodefull

import "github.com/rancher/rancher-cli/internal/clierr"

func attach(c []clierr.Candidate) *clierr.Error {
	return clierr.New(clierr.CodeValidation, "invalid").WithCandidates(c)
}
