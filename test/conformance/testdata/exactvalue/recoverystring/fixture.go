// Package recoverystring is a fixture of the exact-value scans.
package recoverystring

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// recovery sets a command-only field from String.
func recovery(entry string) clierr.ConfigRecovery {
	c := clierr.NewCommand("config delete-server").Arg(entry)
	return clierr.ConfigRecovery{Entry: entry, Command: c.String()} // want 4.9
}
