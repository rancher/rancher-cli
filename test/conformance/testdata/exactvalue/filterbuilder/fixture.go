// Package filterbuilder is a fixture of the exact-value scans.
package filterbuilder

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
)

// withCommand filters a text that holds a command.
func withCommand(name string) string {
	c := clierr.NewCommand("get users").Arg(name)
	return output.Printable("run " + c.String()) // want 3b
}
