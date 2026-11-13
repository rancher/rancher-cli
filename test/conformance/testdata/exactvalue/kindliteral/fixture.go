// Package kindliteral is a fixture of the exact-value scans.
package kindliteral

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
)

// build makes a kind outside the table.
func build(name string) clierr.Command {
	return clierr.NewCommand("get").Word(kinds.Kind{Name: name}.Word()) // want 4.1
}
