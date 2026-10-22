package fixture

import (
	"context"
	"io"
	"net/http"
)

func build(ctx context.Context, newRequest func(context.Context, string, string, io.Reader) (*http.Request, error)) {
	_, _ = newRequest(ctx, http.MethodPost, "https://example.com/apis", nil)
}

func use(ctx context.Context) {
	build(ctx, http.NewRequestWithContext)
}
