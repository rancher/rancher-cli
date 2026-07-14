// deletecluster.go implements the D-6 override for the provisioningcluster
// row: `delete provisioning-cluster` ships even though the row stays
// Deletable: false (spec D-R1-3 — the row's Deletable flag governs generic
// verb GENERATION, not whether a bespoke command may exist for the kind).
// The command wraps resources.NewDelete's own Plan choreography (Route ->
// Get -> rv capture -> dry-run delete -> Effect) unchanged, adding exactly
// two things before/after it: a literal-name refusal for "local", and a
// cascade warning appended to the resulting Effect.
package provisioning

import (
	"context"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
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
// refuses. Deliberately a plain string comparison against the CLI argument,
// NOT a resolution of the fetched object's own linkage back to the local
// management cluster: the brief for D-6 scopes the guard to the literal name
// "local" only, so the refusal short-circuits before any client call (no
// Get, no dry-run) rather than needing to fetch the object first to decide.
const localClusterName = "local"

type deleteProvisioningClusterCmd struct {
	inner runtime.MutatingCommand
}

// NewDeleteProvisioningCluster builds the delete override for the
// provisioningcluster row. clients is passed straight through to
// resources.NewDelete, which performs the entire real choreography
// (Route/Get/rv/dry-run/exec) unmodified; this command only wraps that
// Plan with the local-refusal check and the cascade warning.
func NewDeleteProvisioningCluster(k kinds.Kind, clients engine.ClusterClientFactory) runtime.Command {
	inner, ok := resources.NewDelete(k, clients).(runtime.MutatingCommand)
	if !ok {
		// Unreachable: resources.NewDelete always returns a MutatingCommand.
		panic("resources.NewDelete did not return a runtime.MutatingCommand")
	}
	return &deleteProvisioningClusterCmd{inner: inner}
}

// Describe delegates to the wrapped generic delete's Descriptor verbatim —
// same Name ("delete provisioning-cluster"), Args, Flags, Destructive — so
// the row's schema/completion/routing surface is unchanged from what
// resources.NewDelete(k, ...) would have generated; only Plan's behavior
// differs.
func (c *deleteProvisioningClusterCmd) Describe() runtime.Descriptor {
	return c.inner.Describe()
}

func (c *deleteProvisioningClusterCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if len(args) > 0 && args[0] == localClusterName {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, "refusing to delete the local cluster").
			WithHint("the local cluster is not a provisioning-cluster resource that can be removed this way")
	}

	eff, exec, err := c.inner.Plan(ctx, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	eff.Warnings = append(eff.Warnings, cascadeWarning)
	return eff, exec, nil
}

var _ runtime.MutatingCommand = (*deleteProvisioningClusterCmd)(nil)
