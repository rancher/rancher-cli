// Package typedline is a fixture of the exact-value scans.
package typedline

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// hint uses the typed line builder outside the line refusal hint.
func hint(word string) string {
	return clierr.NewTypedLine().Bare(word).Field() // want 4.5 4.6
}
