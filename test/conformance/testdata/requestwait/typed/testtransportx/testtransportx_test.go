package testtransportx

import (
	"net/http"
)

func wait(r *http.Request) {
	<-r.Context().Done()
}
