package rawqueryassign

import "net/http"

func strip(req *http.Request) {
	req.URL.RawQuery = ""
}
