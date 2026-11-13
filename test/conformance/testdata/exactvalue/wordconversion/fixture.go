// Package wordconversion is a fixture of the exact-value scans.
package wordconversion

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// build converts a runtime value to command words.
func build(name string) clierr.Command {
	return clierr.NewCommand(clierr.CommandWord(name)) // want 4.1
}
