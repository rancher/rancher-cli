// Package originconcat is a fixture of the exact-value scans.
package originconcat

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// build joins two values into one.
func build(prefix, name string) clierr.Command {
	return clierr.NewCommand("get users").Arg(prefix + name) // want 3a
}
