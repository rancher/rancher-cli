package requestliteralalias

import "net/http"

type escapeReq = http.Request

var _ = escapeReq{Method: http.MethodPost}
