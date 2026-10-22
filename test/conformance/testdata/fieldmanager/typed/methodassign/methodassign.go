package methodassign

import "net/http"

func flip(req *http.Request) {
	req.Method = http.MethodPost
}
