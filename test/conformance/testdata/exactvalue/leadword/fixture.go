// Package leadword is a fixture of the exact-value scans.
package leadword

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// text leaves a lead-in word in the prose before the Lead.
func text(c clierr.Command) string {
	return clierr.Text("release web did not reach a revision; read the operation with ", clierr.Lead("x: "), c) // want 4.2
}
