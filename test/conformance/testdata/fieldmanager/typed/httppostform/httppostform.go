package httppostform

import (
	"net/http"
	"net/url"
)

func send(c *http.Client) {
	_, _ = c.PostForm("https://example.com/apis", url.Values{})
}
