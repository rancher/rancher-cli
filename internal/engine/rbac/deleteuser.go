// deleteuser.go implements the delete override for the user kind row: the
// row becomes Deletable and the generic delete provides the full dry-run/
// apply/--yes gating; this wrapper adds a cascade warning and a self-delete
// guard. The guard uses the ext.cattle.io SelfUser probe to resolve the
// current user's ID at plan time — the same probe the create-cluster command
// uses for creatorId stamping — and refuses deletion when the target matches
// the authenticated caller's own record. Guard failure (probe unavailable or
// empty ID) is non-blocking: the guard skips and the inner Plan proceeds.
package rbac

import (
	"context"

	"github.com/rancher/rancher-cli/internal/client/selfuser"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// cascadeWarning: deleting a user removes the identity record and
// Rancher's controllers remove the user's role bindings (cluster and
// project memberships, global role bindings) with it.
const userCascadeWarning = "deleting this user removes the identity record; Rancher's controllers remove the user's role bindings (cluster memberships, project memberships, global role bindings) with it"

type deleteUserCmd struct {
	inner   runtime.MutatingCommand
	clients engine.ClusterClientFactory
}

// NewDeleteUser builds the delete override for the user kind row. clients is
// passed straight through to resources.NewDelete, which performs the entire
// real choreography (Route/Get/rv/dry-run/exec) unmodified; this command
// adds two things: a self-delete guard (SelfUser probe) and the cascade
// warning appended to the resulting Effect.
func NewDeleteUser(k kinds.Kind, clients engine.ClusterClientFactory) runtime.Command {
	inner, ok := resources.NewDelete(k, clients).(runtime.MutatingCommand)
	if !ok {
		// Unreachable: resources.NewDelete always returns a MutatingCommand.
		panic("resources.NewDelete did not return a runtime.MutatingCommand")
	}
	return &deleteUserCmd{inner: inner, clients: clients}
}

// Describe delegates to the wrapped generic delete's Descriptor verbatim —
// same Name ("delete user"), Args, Flags, Destructive — so the row's schema/
// completion/routing surface is unchanged from what resources.NewDelete(k, …)
// would have generated; only Plan's behavior differs.
func (c *deleteUserCmd) Describe() runtime.Descriptor {
	return c.inner.Describe()
}

func (c *deleteUserCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	if err := c.guardSelfDelete(ctx, in); err != nil {
		return safety.Effect{}, nil, err
	}

	eff, exec, err := c.inner.Plan(ctx, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	eff.Warnings = append(eff.Warnings, userCascadeWarning)
	return eff, exec, nil
}

// guardSelfDelete probes the current user's identity via the SelfUser
// endpoint and refuses deletion when the target arg matches the authenticated
// caller's own user ID. A probe failure or empty ID is non-blocking — the
// guard skips silently rather than preventing a legitimate delete when the
// identity endpoint is unavailable (e.g. under a restricted token or on an
// older Rancher that lacks the ext.cattle.io group).
func (c *deleteUserCmd) guardSelfDelete(ctx context.Context, in runtime.Input) error {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return nil
	}
	client, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return nil // Probe inaccessible; skip guard.
	}
	probe, err := client.Create(ctx, selfuser.GVR, "", selfuser.New(), false)
	if err != nil {
		return nil // Probe failed; skip guard.
	}
	currentID := selfuser.UserID(probe)
	if currentID == "" || currentID != args[0] {
		return nil
	}
	return clierr.New(clierr.CodeValidation,
		"refusing to delete user "+args[0]+": deleting your own user record locks this CLI out").
		WithHint("switch to a different admin account, then delete this user — another admin must do it")
}

var _ runtime.MutatingCommand = (*deleteUserCmd)(nil)
