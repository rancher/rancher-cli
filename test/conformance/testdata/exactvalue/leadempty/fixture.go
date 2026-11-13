// Package leadempty is a fixture of the exact-value scans.
package leadempty

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// text gives an empty Lead.
func text(c clierr.Command) string {
	return clierr.Text("release web did not reach a revision; read the operation with ", clierr.Lead(""), c) // want 4.2
}
