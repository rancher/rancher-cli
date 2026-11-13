// Package wordsscan is a fixture of the exact-value scans.
package wordsscan

import (
	"fmt"
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// build scans command words.
func build(in string) clierr.Command {
	var w clierr.CommandWord
	_, _ = fmt.Sscan(in, &w) // want 4.1
	return clierr.NewCommand(w)
}
