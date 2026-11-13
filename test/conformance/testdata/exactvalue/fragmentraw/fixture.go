// Package fragmentraw is a fixture of the exact-value scans.
package fragmentraw

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// text asks for a flag value with the raw value.
func text(names []string) *clierr.Error {
	return clierr.New("FIXTURE", clierr.NewCommand("auth login").String()+"; or add --auth-provider "+names[0]) // want 4.8
}
