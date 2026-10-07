// A path joined from constants with strings.Join.
package stringsjoin

import (
	"context"
	"net/http"
	"strings"
)

func Read(ctx context.Context) {
	_, _ = http.NewRequestWithContext(ctx, http.MethodGet, strings.Join([]string{"/k8s/clusters/local/apis", "management.cattle.io", "v3", "clusters"}, "/"), nil)
}
