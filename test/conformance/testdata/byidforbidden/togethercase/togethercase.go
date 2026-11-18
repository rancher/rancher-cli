package togethercase

import "net/http"

func refused(st int) bool {
	switch st {
	case http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}
