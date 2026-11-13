// Package quotevar is a fixture of the exact-value scans.
package quotevar

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// deleteServer is a command prefix built on the marker.
var deleteServer = clierr.Rancher + " config delete-server"

// text appends a quoted value to the prefix.
func text(name string) *clierr.Error {
	return clierr.New("FIXTURE", deleteServer+" "+clierr.ShellQuote(name)) // want 4.4 4.5
}
