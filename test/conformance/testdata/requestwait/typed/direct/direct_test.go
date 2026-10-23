package direct

import (
	"net/http"
)

func wait(w http.ResponseWriter, r *http.Request) {
	<-r.Context().Done()
}
