package ifacecontext

import (
	"context"
	"net/http"
)

func wait(r *http.Request) {
	var c interface{ Context() context.Context } = r
	<-c.Context().Done()
}

type holder struct {
	c interface{ Context() context.Context }
}

func waitField(r *http.Request) {
	h := holder{c: r}
	<-h.c.Context().Done()
}

func waitParam(c interface{ Context() context.Context }) {
	<-c.Context().Done()
}

func passParam(r *http.Request) {
	waitParam(r)
}
