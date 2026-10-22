package fixture

import (
	"context"
	"net/http"
)

const method = "DELETE"

func build(ctx context.Context) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, method, "https://example.com/apis", nil)
}
