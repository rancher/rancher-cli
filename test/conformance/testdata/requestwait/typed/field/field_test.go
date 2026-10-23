package field

import (
	"context"
	"net/http"
)

type holder struct{ ctx context.Context }

func literal(w http.ResponseWriter, r *http.Request) {
	h := holder{ctx: r.Context()}
	<-h.ctx.Done()
}

func assigned(w http.ResponseWriter, r *http.Request) {
	var h holder
	h.ctx = r.Context()
	<-h.ctx.Done()
}
