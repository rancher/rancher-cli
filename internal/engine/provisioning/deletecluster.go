// deletecluster.go implements the D-6 override for the provisioningcluster
// row: `delete provisioning-cluster` ships even though the row stays
// Deletable: false (spec D-R1-3 — the row's Deletable flag governs generic
// verb GENERATION, not whether a bespoke command may exist for the kind).
// The command wraps resources.NewGuardedDelete's Plan sequence (Route ->
// Get -> rv capture -> dry-run delete -> Effect) unchanged, adding exactly
// two things after it: a literal-name refusal for "local", and a cascade
// warning appended to the resulting Effect. The guard runs post-inner-Plan,
// so a refused "local" delete has already completed the harmless dry-run.
package provisioning

import (
	"context"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// cascadeWarning is the advisory note appended to every non-refused
// provisioning-cluster delete's Effect.Warnings (never a gate — Task 2's
// Warnings contract). It states the real cascade: the provisioning
// controller's remove path deletes the linked CAPI Cluster, which per
// cluster-api's own ownership graph takes down that Cluster's Machines and
// (for an RKE2/K3s control plane) its RKEControlPlane object, and separately
// removes the corresponding management.cattle.io Cluster
// (rancher provisioningv2 remove paths; audit row 6). The exact upstream
// source file/line is not reproduced here verbatim — this line is written to
// be factually accurate to that removal path rather than quoting the audit
// document.
const cascadeWarning = "deleting this provisioning cluster cascades: the provisioning controller removes the linked CAPI Cluster, which takes its Machines and RKEControlPlane with it, and separately removes the corresponding management Cluster"

// localClusterName is the literal argument value NewDeleteProvisioningCluster
// refuses. A plain string comparison against the CLI argument; the guard runs
// after inner.Plan confirms the object's existence, so the refusal requires
// the "local" provisioning-cluster object to be present.
const localClusterName = "local"

// NewDeleteProvisioningCluster builds the delete override for the
// provisioningcluster row. clients is passed straight through to
// resources.NewGuardedDelete, which performs the entire real sequence
// (Route/Get/rv/dry-run/exec) unmodified; this command only wraps that
// Plan with the local-refusal check and the cascade warning.
func NewDeleteProvisioningCluster(k kinds.Kind, clients engine.ClusterClientFactory) runtime.Command {
	guard := func(_ context.Context, in runtime.Input) error {
		if args := in.Args(); len(args) > 0 && args[0] == localClusterName {
			return clierr.New(clierr.CodeValidation,
				"refusing to delete the local cluster; it is not a provisioning-cluster resource that can be removed this way")
		}
		return nil
	}
	return resources.NewGuardedDelete(k, clients, guard, cascadeWarning)
}
