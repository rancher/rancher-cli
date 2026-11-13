// Package wordreflect is a fixture of the exact-value scans.
package wordreflect

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
	"reflect"
)

// build converts by reflection.
func build(name string) clierr.Command {
	w := reflect.ValueOf(name).Convert(reflect.TypeFor[clierr.CommandWord]()).Interface().(clierr.CommandWord) // want 4.1
	return clierr.NewCommand(w)
}
