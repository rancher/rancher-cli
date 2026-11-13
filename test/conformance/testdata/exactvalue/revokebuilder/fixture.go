// Package revokebuilder is a fixture of the exact-value scans.
package revokebuilder

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// text builds a token delete by hand.
func text(name string) clierr.Command {
	return clierr.NewCommand("delete token").Arg(name).Flag("--apply").Flag("--yes") // want 4.7
}
