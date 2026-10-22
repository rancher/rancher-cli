package literaltotal

import "github.com/rancher/rancher-cli/internal/clierr"

func build() clierr.Error {
	return clierr.Error{Code: clierr.CodeNotFound, CandidatesTotal: 12}
}
