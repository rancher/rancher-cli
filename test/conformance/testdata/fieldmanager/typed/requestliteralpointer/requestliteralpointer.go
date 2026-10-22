package requestliteralpointer

import "net/http"

var _ = []*http.Request{{Method: http.MethodPost}}
