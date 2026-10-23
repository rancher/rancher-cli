//go:build e2e

package tagged

import (
	"net/http"
)

func wait(r *http.Request) {
	<-r.Context().Done()
}
