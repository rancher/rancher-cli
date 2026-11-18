package forbiddenorother

import "net/http"

func refused(st int) bool { return st == http.StatusForbidden || st == http.StatusInternalServerError }
