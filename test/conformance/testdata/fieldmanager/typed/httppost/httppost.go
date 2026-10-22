package httppost

import (
	"net/http"
	"strings"
)

func send(c *http.Client) {
	_, _ = c.Post("https://example.com/apis", "application/json", strings.NewReader("{}"))
}
