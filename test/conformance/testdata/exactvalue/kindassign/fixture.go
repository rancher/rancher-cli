// Package kindassign is a fixture of the exact-value scans.
package kindassign

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
)

// build writes the name of a looked-up kind.
func build(name string) clierr.Command {
	k, _ := kinds.Lookup("user")
	k.Name = name // want 4.1
	return clierr.NewCommand("get").Word(k.Word())
}
