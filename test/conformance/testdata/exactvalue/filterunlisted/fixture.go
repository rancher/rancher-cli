// Package filterunlisted is a fixture of the exact-value scans.
package filterunlisted

import (
	"github.com/rancher/rancher-cli/internal/output"
)

// unlisted filters a text with no allowlist row.
func unlisted(s string) string {
	return output.Printable(s) // want 3b
}
