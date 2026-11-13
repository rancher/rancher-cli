// Package leadalone is a fixture of the exact-value scans.
package leadalone

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// text ends with a Lead.
func text() string {
	return clierr.Text("release web did not reach a revision", clierr.Lead("; read the operation with ")) // want 4.2
}
