package separatetests

import "net/http"

func refused(st, other int) (bool, bool) {
	return st == http.StatusForbidden, other == http.StatusNotFound
}

func both(st, other int) bool { return st == http.StatusForbidden && other == http.StatusNotFound }
