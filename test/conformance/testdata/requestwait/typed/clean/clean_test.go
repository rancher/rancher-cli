package clean

import (
	"net/http"

	"github.com/rancher/rancher-cli/internal/testtransport"
)

func wait(w http.ResponseWriter, r *http.Request) {
	testtransport.HangAndAbort(r)
}
