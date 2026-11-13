// Package wordgeneric is a fixture of the exact-value scans.
package wordgeneric

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// conv converts any string to a string type.
func conv[T ~string](s string) T { return T(s) }

// build converts a runtime value through a type parameter.
func build(name string) clierr.Command {
	return clierr.NewCommand(conv[clierr.CommandWord](name)) // want 4.1
}
