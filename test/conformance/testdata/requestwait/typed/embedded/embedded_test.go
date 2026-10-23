package embedded

import (
	"net/http"
)

type wrapped struct{ *http.Request }

func wait(w wrapped) {
	<-w.Context().Done()
}
