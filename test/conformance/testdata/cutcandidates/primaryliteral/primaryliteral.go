package primaryliteral

import "github.com/rancher/rancher-cli/internal/clierr"

func build(c []clierr.Candidate) clierr.PrimaryFailure {
	return clierr.PrimaryFailure{Candidates: c}
}
