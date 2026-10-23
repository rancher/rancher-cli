package helperparam

import (
	"context"
	"net/http"
)

func holdOn(ctx context.Context) {
	<-ctx.Done()
}

func wait(w http.ResponseWriter, r *http.Request) {
	holdOn(r.Context())
}
