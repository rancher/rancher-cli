package fixture

import (
	"context"
	h "net/http"
)

func build(ctx context.Context) (*h.Request, error) {
	return h.NewRequestWithContext(ctx, h.MethodPost, "https://example.com/apis", nil)
}
