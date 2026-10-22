package urlstar

import (
	"net/http"
	"net/url"
)

func replace(req *http.Request) {
	*req.URL = url.URL{Path: "/x"}
}
