// Package textpart is a fixture of the exact-value scans.
package textpart

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// text passes a part of another type.
func text() string {
	return clierr.Text("x", clierr.CommandWord("y")) // want 4.3
}
