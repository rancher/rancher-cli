package readrequest

import (
	"bufio"
	"net/http"
	"strings"
)

func read() (*http.Request, error) {
	return http.ReadRequest(bufio.NewReader(strings.NewReader("POST /apis/x HTTP/1.1\r\nHost: h\r\n\r\n")))
}
