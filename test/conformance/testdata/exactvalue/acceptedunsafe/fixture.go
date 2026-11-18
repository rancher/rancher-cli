// Package acceptedunsafe is a fixture of the exact-value scans.
package acceptedunsafe

import (
	"strings"

	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// called tests each rune of a text.
func called(s string) bool {
	for _, r := range s {
		if clierr.IsUnsafeToDisplay(r) {
			return true
		}
	}
	return false
}

// asValue hands the test to a function that looks for the first rune it
// matches.
func asValue(s string) bool {
	return strings.IndexFunc(s, clierr.IsUnsafeToDisplay) >= 0
}
