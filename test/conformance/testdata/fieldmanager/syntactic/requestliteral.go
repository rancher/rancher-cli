package fixture

import (
	"net/http"
	"net/url"
)

func build(u *url.URL) *http.Request {
	return &http.Request{Method: http.MethodPost, URL: u}
}
