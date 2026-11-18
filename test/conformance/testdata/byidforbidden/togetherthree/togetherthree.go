package togetherthree

import "net/http"

func refused(st int) bool {
	return st == http.StatusInternalServerError || st == http.StatusForbidden || st == http.StatusNotFound
}
