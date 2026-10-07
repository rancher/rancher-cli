// A path formatted from a constant base with fmt.Sprintf.
package sprintf

import (
	"context"
	"fmt"
	"net/http"
)

const base = "/k8s/clusters/local/apis/management.cattle.io/v3"

func Read(ctx context.Context) {
	_, _ = http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/clusters", base), nil)
}
