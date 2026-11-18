package togethermixed

import (
	"net/http"

	"github.com/rancher/rancher-cli/internal/clierr"
)

func refused(st int, ce *clierr.Error) bool {
	return (st == http.StatusForbidden) || ce.Code == clierr.CodeNotFound
}
