package forbiddenonly

import "net/http"

func refused(st int) bool { return st == http.StatusForbidden }
