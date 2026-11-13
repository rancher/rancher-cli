// Package leadprose is a fixture of the exact-value scans.
package leadprose

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// text writes the words that introduce a command as prose.
func text(c clierr.Command) string {
	return clierr.Text("release web did not reach a revision; read the operation with ", c) // want 4.2
}
