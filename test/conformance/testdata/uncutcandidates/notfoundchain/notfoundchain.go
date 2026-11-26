package notfoundchain

import "github.com/rancher/rancher-cli/internal/clierr"

func attach(c []clierr.Candidate) *clierr.Error {
	return clierr.New(clierr.CodeNotFound, "not found").WithHint("try again").WithRetryable(false).WithCandidates(c)
}
