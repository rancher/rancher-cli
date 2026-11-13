// Package quotereturn is a fixture of the exact-value scans.
package quotereturn

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// quoted returns a quoted value.
func quoted(v string) string {
	return clierr.ShellQuote(v) // want 4.4
}

// text puts the quoted value in a command.
func text(name string) *clierr.Error {
	return clierr.New("FIXTURE", clierr.Rancher+" get users "+quoted(name)) // want 4.5
}
