// Package quoteparam is a fixture of the exact-value scans.
package quoteparam

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// withName writes a quoted value after a parameter.
func withName(prefix, name string) string {
	return prefix + " " + clierr.ShellQuote(name) // want 4.4
}

// text passes a command start as the parameter.
func text(name string) string {
	return "run " + withName(clierr.NewCommand("get users").String(), name)
}
