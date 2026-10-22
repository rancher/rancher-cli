package urlassign

import (
	"net/http"
	"net/url"
)

func swap(req *http.Request, u *url.URL) {
	req.URL = u
}
