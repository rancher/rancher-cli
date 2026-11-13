// Package originfield is a fixture of the exact-value scans.
package originfield

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
)

// revoke filters a field of the reference before the revoke.
func revoke(ref clierr.TokenRef) clierr.Command {
	ref.Name = output.Printable(ref.Name) // want 3a
	return clierr.Revoke(ref)
}
