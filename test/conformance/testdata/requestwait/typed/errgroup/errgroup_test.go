package errgroup

import (
	"net/http"

	"golang.org/x/sync/errgroup"
)

func wait(r *http.Request) {
	g, ctx := errgroup.WithContext(r.Context())
	_ = g
	<-ctx.Done()
}
