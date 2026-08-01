// deleteuser.go implements the delete override for the user kind row: the
// row becomes Deletable and the generic delete provides the full dry-run/
// apply/--yes gating; this wrapper adds a cascade warning and a self-delete
// guard. The guard runs after inner.Plan confirms the target's existence,
// using the ext.cattle.io SelfUser probe to resolve the current user's ID
// and refusing deletion when the target matches the authenticated caller's
// own record. A probe that returns NOT_FOUND (the ext.cattle.io group absent
// on older Rancher) skips the guard as a legitimately unsupported endpoint.
// A probe that returns an empty identity (success, but no userID field)
// refuses retryably — an unverifiable identity must cost a retry rather than
// risk a lockout, same as any other probe failure. Any other probe or Cluster
// failure refuses with a retryable GENERIC error: for an irreversible identity
// deletion a transient failure must cost a retry, not risk a lockout.
package rbac

import (
	"context"
	"errors"
	"fmt"

	"github.com/rancher/rancher-cli/internal/client/selfuser"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
)

// cascadeWarning: deleting a user removes the identity record and
// Rancher's controllers remove the user's role bindings (cluster and
// project memberships, global role bindings) with it.
const userCascadeWarning = "deleting this user removes the identity record; Rancher's controllers remove the user's role bindings (cluster memberships, project memberships, global role bindings) with it"

// NewDeleteUser builds the delete override for the user kind row. clients is
// passed straight through to resources.NewGuardedDelete, which runs the inner
// Plan choreography (Route/Get/rv/dry-run/exec) first and then runs the
// self-delete guard and appends the cascade warning to the resulting Effect.
func NewDeleteUser(k kinds.Kind, clients engine.ClusterClientFactory) runtime.Command {
	return resources.NewGuardedDelete(k, clients, func(ctx context.Context, in runtime.Input) error {
		return guardSelfDelete(ctx, clients, in)
	}, userCascadeWarning)
}

// guardSelfDelete probes the current user's identity via the SelfUser
// endpoint and refuses deletion when the target arg matches the authenticated
// caller's own user ID. Cancellation propagates. A NOT_FOUND probe error
// means the ext.cattle.io endpoint is absent (older Rancher) — skip the
// guard. A probe that succeeds but carries no userID refuses retryably: an
// unverifiable identity must cost a retry, not risk a lockout. A transient
// API-aggregation flap can momentarily drop the ext.cattle.io group from
// discovery and produce the same NOT_FOUND as a permanently absent endpoint,
// so the NOT_FOUND skip is a deliberate availability tradeoff — the guard is
// defense-in-depth; the destructive gates (--dry-run preview, --apply, --yes,
// cascade warning) remain the primary protection.
func guardSelfDelete(ctx context.Context, clients engine.ClusterClientFactory, in runtime.Input) error {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return nil
	}
	client, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return guardVerifyRefusal(err)
	}
	probe, err := client.Create(ctx, selfuser.GVR, "", selfuser.New(), false)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if clierr.IsNotFound(err) {
			return nil // ext.cattle.io absent on older Rancher — unsupported endpoint, skip
		}
		return guardVerifyRefusal(err)
	}
	currentID := selfuser.UserID(probe)
	if currentID == "" {
		return guardVerifyRefusal(errors.New("probe returned no identity"))
	}
	if currentID != args[0] {
		return nil
	}
	return clierr.New(clierr.CodeValidation,
		"refusing to delete user "+args[0]+": deleting your own user record locks this CLI out").
		WithHint("switch to a different admin account, then delete this user — another admin must do it")
}

// guardVerifyRefusal returns the retryable GENERIC error used when the
// self-delete guard cannot be verified. The cause is embedded for diagnostics.
func guardVerifyRefusal(cause error) *clierr.Error {
	return clierr.Retryable(clierr.CodeGeneric,
		fmt.Sprintf("could not verify the target is not your own user: %v", cause)).
		WithHint("the self-delete guard could not run — retry, or have another admin delete this user")
}
