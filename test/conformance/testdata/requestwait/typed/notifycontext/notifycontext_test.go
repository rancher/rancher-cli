package notifycontext

import (
	"net/http"
	"os"
	"os/signal"
)

func wait(r *http.Request) {
	ctx, stop := signal.NotifyContext(r.Context(), os.Interrupt)
	defer stop()
	<-ctx.Done()
}
