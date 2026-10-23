// Package nethttp is a fixture for the import guard: its closure holds
// net/http.
package nethttp

import "net/http"

// Status is a use of the import.
func Status() int { return http.StatusOK }
