// Package originparam is a fixture of the exact-value scans.
package originparam

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
)

// build passes its parameter to the builder.
func build(name string) clierr.Command {
	return clierr.NewCommand("get users").Arg(name)
}

// caller filters the value it passes.
func caller(raw string) clierr.Command {
	return build(output.Printable(raw)) // want 3a
}
