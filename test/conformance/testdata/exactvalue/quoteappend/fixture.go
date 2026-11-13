// Package quoteappend is a fixture of the exact-value scans.
package quoteappend

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
	"strings"
)

// text builds a command as a []string by append.
func text(name string) string {
	args := []string{clierr.Rancher, "get", "users"}
	args = append(args, clierr.ShellQuote(name)) // want 4.4
	return strings.Join(args, " ")
}
