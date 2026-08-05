package observe

import (
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// gitRepoCascadeWarning is the advisory note appended to every gitrepo
// delete Effect: removing the GitRepo removes the workloads it deployed.
// Advisory only — Destructive is already true via the wrapped generic
// delete, so the --yes gate needs no separate flag.
const gitRepoCascadeWarning = "deleting this GitRepo cascades: Fleet removes its Bundles and their BundleDeployments, which uninstalls the deployed workloads from every target cluster"

// NewDeleteGitRepo wraps the generic delete (no guard) so the Effect carries
// the cascade warning — the same treatment delete provisioning-cluster gives
// its blast radius.
func NewDeleteGitRepo(k kinds.Kind, clients engine.ClusterClientFactory) runtime.Command {
	return resources.NewGuardedDelete(k, clients, nil, gitRepoCascadeWarning)
}
