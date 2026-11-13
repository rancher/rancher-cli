// Package hintcommand is a fixture of the exact-value scans.
package hintcommand

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// fail sets a built command as a plain hint.
func fail(name string) *clierr.Error {
	c := clierr.NewCommand("get users").Arg(name)
	return clierr.New("NOT_FOUND", "no user").WithHint(c.String()) // want 4.9
}
