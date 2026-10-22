package fixture

import (
	"context"
	"net/http"
)

func build(ctx context.Context) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com/apis", nil)
}
