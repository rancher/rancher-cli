package derivehelper

import (
	"context"
	"net/http"
)

func derive(ctx context.Context) context.Context {
	return ctx
}

func wait(r *http.Request) {
	ctx := derive(r.Context())
	<-ctx.Done()
}
