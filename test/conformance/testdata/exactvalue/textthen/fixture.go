// Package textthen is a fixture of the exact-value scans.
package textthen

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// text follows a Text result with the next step.
func text(c, d clierr.Command) *clierr.Error {
	return clierr.New("FIXTURE", clierr.Text("first", clierr.Lead(": "), c)+"; then "+d.Field()) // want 4.6
}
