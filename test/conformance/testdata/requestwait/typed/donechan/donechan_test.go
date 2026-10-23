package donechan

import (
	"net/http"
)

func wait(w http.ResponseWriter, r *http.Request) {
	done := r.Context().Done()
	<-done
}
