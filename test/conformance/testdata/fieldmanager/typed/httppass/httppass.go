package httppass

import (
	"net/http"
	"net/url"
)

func read(c *http.Client, u *url.URL) {
	_, _ = c.Get("https://example.com/apis")
	_, _ = c.Do(&http.Request{URL: u})
	_ = http.StatusOK
}
