package copyof

import "github.com/rancher/rancher-cli/internal/clierr"

func pass(from *clierr.Error) *clierr.Error {
	return clierr.New(clierr.CodeRBAC, "refused").WithCandidatesOf(from)
}
