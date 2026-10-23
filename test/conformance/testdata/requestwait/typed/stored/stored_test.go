package stored

import (
	"net/http"
)

func wait(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	<-ctx.Done()
}
