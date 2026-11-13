// Package quoteloop is a fixture of the exact-value scans.
package quoteloop

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
	"strings"
)

// text quotes the elements of a slice in a loop, then joins them into a
// command.
func text(names []string) *clierr.Error {
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = clierr.ShellQuote(n) // want 4.4
	}
	return clierr.New("FIXTURE", clierr.Rancher+" get users "+strings.Join(parts, " ")) // want 4.5
}
