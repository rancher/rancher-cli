package reqhelper

import (
	"context"
	"net/http"
)

func contextOf(r *http.Request) context.Context {
	return r.Context()
}

func wait(r *http.Request) {
	<-contextOf(r).Done()
}
