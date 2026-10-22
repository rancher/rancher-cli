package fixture

import (
	"context"
	"net/http"
)

func build(ctx context.Context) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodPatch, "https://example.com/apis", nil)
}
