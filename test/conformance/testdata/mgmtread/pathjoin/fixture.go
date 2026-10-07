// A path joined from constants with path.Join.
package pathjoin

import (
	"context"
	"net/http"
	"path"
)

func Read(ctx context.Context) {
	_, _ = http.NewRequestWithContext(ctx, http.MethodGet, path.Join("/k8s/clusters/local/apis", "management.cattle.io", "v3", "clusters"), nil)
}
