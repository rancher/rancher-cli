package derived

import (
	"context"
	"net/http"
	"time"
)

func wait(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
	}
}
