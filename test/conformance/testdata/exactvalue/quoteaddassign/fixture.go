// Package quoteaddassign is a fixture of the exact-value scans.
package quoteaddassign

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// text adds a quoted value to a command with +=.
func text(name string) *clierr.Error {
	cmd := clierr.Rancher + " get users"
	cmd += " " + clierr.ShellQuote(name) // want 4.4 4.5
	return clierr.New("FIXTURE", "run "+cmd)
}
