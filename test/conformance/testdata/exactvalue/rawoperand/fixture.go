// Package rawoperand is a fixture of the exact-value scans.
package rawoperand

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// text writes a raw value after a built command.
func text(name string) *clierr.Error {
	c := clierr.NewCommand("get users")
	return clierr.New("FIXTURE", c.String()+" "+name) // want 4.5
}
