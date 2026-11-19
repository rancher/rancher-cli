package structfield

import "github.com/rancher/rancher-cli/internal/clierr"

type holder struct {
	named clierr.Request
	clierr.Request
}
