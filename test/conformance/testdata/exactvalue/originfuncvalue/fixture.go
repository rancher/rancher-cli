// Package originfuncvalue is a fixture of the exact-value scans.
package originfuncvalue

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// build is used as a value, so its callers are not known.
func build(name string) clierr.Command { // want 3a
	return clierr.NewCommand("get users").Arg(name)
}

// Builder holds build as a value.
var Builder = build
