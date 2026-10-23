package names

import (
	"net/http"
)

func byIn(in *http.Request) {
	<-in.Context().Done()
}

func byReq(req *http.Request) {
	<-req.Context().Done()
}

func byRequest(request *http.Request) {
	<-request.Context().Done()
}
