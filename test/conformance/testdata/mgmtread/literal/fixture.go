// A literal path to the local management clusters API.
package literal

import (
	"context"
	"net/http"
)

func Read(ctx context.Context) {
	_, _ = http.NewRequestWithContext(ctx, http.MethodGet, "/k8s/clusters/local/apis/management.cattle.io/v3/clusters", nil)
}
