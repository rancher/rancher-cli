package readliteral

import "github.com/rancher/rancher-cli/internal/clierr"

func build(c []clierr.Candidate) *clierr.Error {
	return &clierr.Error{Code: clierr.CodeNotFound, Message: "not found", Candidates: c}
}
