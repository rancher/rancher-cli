// Package filteredvet is a fixture of the vet check of the two format
// functions of clierr.Filtered: each has one wrong format, and go vet
// reports both.
package filteredvet

import "github.com/rancher/rancher-cli/internal/clierr"

// wrong writes a string with %d through each function.
func wrong(name, count string) clierr.Filtered {
	var b clierr.Builder
	b.Printf("%d items\n", count)
	return clierr.Sprintf("%d: %s", name, b.Filtered())
}

var _ = wrong
