package urladdress

import "net/http"

func set(any) {}

func flip(req *http.Request) {
	set(&req.URL)
	set(&req.URL.RawQuery)
}
