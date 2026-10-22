package fixture

import (
	"net/http"
	"net/url"
)

func build(u *url.URL) *http.Request {
	return &http.Request{URL: u}
}
