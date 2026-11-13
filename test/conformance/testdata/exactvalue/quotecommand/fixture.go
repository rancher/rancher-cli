// Package quotecommand is a fixture of the exact-value scans.
package quotecommand

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// text quotes a value into a command by hand.
func text(name string) *clierr.Error {
	return clierr.New("FIXTURE", clierr.Rancher+" get users "+clierr.ShellQuote(name)) // want 4.4 4.5
}
