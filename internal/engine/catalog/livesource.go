package catalog

import (
	kindscomplete "github.com/rancher/rancher-cli/internal/engine/kinds/complete"
	"github.com/rancher/rancher-cli/internal/runtime/completion"
)

// LiveSource hands the surface the --repo source: the ClusterRepos of the
// typed cluster that are not disabled. Every other site is declined, --cluster
// among them, which the shared cluster source serves.
func (*chartsCmd) LiveSource() completion.Source { return kindscomplete.ClusterRepoSource() }
