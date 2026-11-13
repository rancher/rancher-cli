// Package twocommands is a fixture of the exact-value scans.
package twocommands

import (
	clierr "github.com/rancher/rancher-cli/internal/clierr"
)

// text joins two commands outside a Sequence.
func text(entry, cluster string) *clierr.Error {
	use := clierr.NewCommand("config use-server").Arg(entry)
	kube := clierr.NewCommand("kubeconfig").FlagValue("--cluster", cluster)
	return clierr.New("FIXTURE", use.String()+"; then "+kube.String()) // want 4.5
}
