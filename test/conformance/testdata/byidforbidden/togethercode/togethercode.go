package togethercode

import "github.com/rancher/rancher-cli/internal/clierr"

func refused(ce *clierr.Error) bool {
	return ce.Code == clierr.CodeNotFound || ce.Code == clierr.CodeRBAC
}
