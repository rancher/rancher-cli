// Package originlocal is a fixture of the exact-value scans.
package originlocal

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
)

// build filters a value into a local before the builder.
func build(name string) clierr.Command {
	v := output.Printable(name) // want 3a
	return clierr.NewCommand("get users").Arg(v)
}
