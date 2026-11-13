// Package origintrim is a fixture of the exact-value scans.
package origintrim

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
	"strings"
)

// build trims a value before the builder.
func build(name string) clierr.Command {
	return clierr.NewCommand("get users").Arg(strings.TrimSpace(name)) // want 3a
}
