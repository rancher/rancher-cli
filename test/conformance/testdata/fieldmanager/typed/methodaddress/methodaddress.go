package methodaddress

import "net/http"

func set(*string) {}

func flip(req *http.Request) {
	set(&req.Method)
}
