package fixture

import (
	"context"
	"net/http"
)

func build(ctx context.Context) {
	_, _ = http.NewRequest(http.MethodGet, "https://example.com/apis", nil)
	_, _ = http.NewRequestWithContext(ctx, http.MethodDelete, "https://example.com/apis", nil)
	_, _ = http.NewRequest("HEAD", "https://example.com/apis", nil)
	_, _ = http.NewRequest(http.MethodOptions, "https://example.com/apis", nil)
}
