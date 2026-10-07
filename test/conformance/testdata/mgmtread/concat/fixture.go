// A path concatenated from a variable and constants.
package concat

import (
	"context"
	"net/http"
)

func Read(ctx context.Context, base string) {
	_, _ = http.NewRequestWithContext(ctx, http.MethodGet, base+"/k8s/clusters/local/apis/management.cattle.io/v3/"+"clusters", nil)
}
