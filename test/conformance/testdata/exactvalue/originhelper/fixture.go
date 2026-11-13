// Package originhelper is a fixture of the exact-value scans.
package originhelper

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// clean changes a value.
func clean(s string) string { return s + "x" }

// build passes a cleaned value to the builder.
func build(name string) clierr.Command {
	return clierr.NewCommand("get users").Arg(clean(name)) // want 3a
}
