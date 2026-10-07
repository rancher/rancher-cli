// A named constant built by concatenating constants.
package constant

import (
	"context"
	"net/http"
)

const clustersPath = "/k8s/clusters/local/apis/" + "management.cattle.io/v3/clusters"

func Read(ctx context.Context) {
	_, _ = http.NewRequestWithContext(ctx, http.MethodGet, clustersPath, nil)
}
