// Package leadconcat is a fixture of the exact-value scans.
package leadconcat

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// text builds a Lead from a runtime value.
func text(name string, c clierr.Command) string {
	return clierr.Text("the user is gone", clierr.Lead("; run it as "+name+": "), c) // want 4.1 4.2
}
