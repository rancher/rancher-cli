// Package stringhelper is a fixture of the exact-value scans.
package stringhelper

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// k returns a command as a string.
func k(id string) string {
	return clierr.NewCommand("kubeconfig").FlagValue("--cluster", id).String() // want 4.6
}

// text joins the helper's command after another.
func text(n, id string) *clierr.Error {
	return clierr.New("FIXTURE", clierr.NewCommand("config use-server").Arg(n).String()+"; then "+k(id))
}
