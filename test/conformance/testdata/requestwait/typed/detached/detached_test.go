package detached

import (
	"context"
	"net/http"
	"time"
)

func plain(r *http.Request) {
	<-context.WithoutCancel(r.Context()).Done()
}

func bounded(r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Second)
	defer cancel()
	<-ctx.Done()
}
