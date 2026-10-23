package selectcase

import (
	"net/http"
	"time"
)

func wait(w http.ResponseWriter, r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(time.Second):
	}
}
