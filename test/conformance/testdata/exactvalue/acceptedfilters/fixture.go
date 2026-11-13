// Package acceptedfilters is a fixture of the exact-value scans.
package acceptedfilters

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/output"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// plain filters a text that holds no command.
func plain(name string) string {
	return output.Printable("the user " + name + " is gone")
}

// remedies filters a text that holds a command with the filter that keeps
// it.
func remedies(name string) string {
	return runtime.PrintableRemedies("run " + clierr.NewCommand("get users").Arg(name).String())
}

// relayedWithReason filters a relayed message under a row with a reason.
func relayedWithReason(err error) string {
	return output.Printable(err.Error())
}
