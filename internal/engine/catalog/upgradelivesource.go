package catalog

import (
	kindscomplete "github.com/rancher/rancher-cli/internal/engine/kinds/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// LiveSource hands the surface the sources of <release>, --repo, --chart and
// --version: the apps of the typed namespace that an upgrade accepts, the
// app's source ClusterRepo, the app's chart, and the versions of that chart
// in the repository's index plus the installed version. Every other site is
// declined, --cluster and --namespace among them, which the shared sources
// serve.
func (c *upgradeCmd) LiveSource() completion.Source {
	return kindscomplete.UpgradeSource(c.Describe().Name)
}
