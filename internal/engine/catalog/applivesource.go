package catalog

import (
	kindscomplete "github.com/rancher/rancher-cli/internal/engine/kinds/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// LiveSource hands the surface the sources of --repo, --chart and --version:
// the ClusterRepos of the typed cluster that are not disabled, and the charts
// and versions of the typed --repo's index. Every other site is declined,
// --cluster and --namespace among them, which the shared sources serve.
func (c *installCmd) LiveSource() completion.Source {
	return kindscomplete.InstallSource(c.Describe().Name)
}
