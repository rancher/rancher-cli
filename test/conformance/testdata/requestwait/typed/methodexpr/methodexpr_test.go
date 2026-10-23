package methodexpr

import (
	"net/http"
)

func wait(r *http.Request) {
	<-(*http.Request).Context(r).Done()
}
