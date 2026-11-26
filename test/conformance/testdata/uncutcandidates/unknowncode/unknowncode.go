package unknowncode

import "github.com/rancher/rancher-cli/internal/clierr"

func attach(code string, c []clierr.Candidate) *clierr.Error {
	return clierr.New(code, "failed").WithCandidates(c)
}
