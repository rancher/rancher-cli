package afterfunc

import (
	"context"
	"net/http"
)

func wait(r *http.Request) {
	done := make(chan struct{})
	context.AfterFunc(r.Context(), func() { close(done) })
	<-done
}
