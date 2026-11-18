// Package unsafeunlisted is a fixture of the exact-value scans.
package unsafeunlisted

import (
	"strings"

	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// called tests each rune of a text, with no row.
func called(s string) bool {
	for _, r := range s {
		if clierr.IsUnsafeToDisplay(r) { // want 3d
			return true
		}
	}
	return false
}

// asValue hands the test to a function that looks for the first rune it
// matches, with no row.
func asValue(s string) bool {
	return strings.IndexFunc(s, clierr.IsUnsafeToDisplay) >= 0 // want 3d
}
