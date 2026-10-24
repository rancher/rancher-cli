// Package httpx is a fixture for the import guard: it imports the HTTP
// client package, which a package that makes no request must not.
package httpx

import "github.com/rancher/rancher-cli/internal/client/httpx"

// Agent is a use of the import.
func Agent() string { return httpx.UserAgent() }
