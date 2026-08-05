// deleteuser.go implements the delete override for the user kind row: the
// row becomes Deletable and the generic delete provides the full dry-run/
// apply/--yes gating; this wrapper adds a cascade warning and a self-delete
// guard. The guard runs PrePlan (see resources.guardedDeleteCmd.prePlan for
// the full tradeoff): on a real Rancher server the users webhook denies a
// self-delete dry-run before a PostPlan guard could fire, surfacing raw
// webhook text with no hint. Running the guard first ensures the curated
// lockout refusal always wins.
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
// passed straight through to resources.NewGuardedDeletePrePlan, which runs the
// self-delete guard before the inner Plan choreography (Route/Get/rv/dry-run/
// exec) so the curated lockout refusal fires before the server's own webhook
// can refuse the dry-run with raw text. The cascade warning is appended to the
// resulting Effect on success.
func NewDeleteUser(k kinds.Kind, clients engine.ClusterClientFactory) runtime.Command {
	return resources.NewGuardedDeletePrePlan(k, clients, func(ctx context.Context, in runtime.Input) error {
		return guardSelfDelete(ctx, clients, in)
	}, userCascadeWarning)
}

// guardSelfDelete probes the current user's identity via the SelfUser
// endpoint and refuses deletion when the target arg matches the authenticated
// caller's own user ID. A client construction failure that carries a clierr
// code passes through unchanged — its own code and retryability stand, and
// the pipeline's toClierr normalization will surface it correctly. A raw
// (non-clierr) construction failure is ambiguous: the guard refuses retryably
// so an unverifiable identity costs a retry, not a lockout. Cancellation
// propagates via the probe call path: if the context is already cancelled,
// client.Create returns a context error and ctx.Err() is non-nil. A NOT_FOUND
// probe error means the ext.cattle.io endpoint is absent (older Rancher) —
// skip the guard. A classified probe error (code other than Generic, a hint,
// or Retryable true) passes through with its identity intact; mapErr's
// catch-all default (CodeGeneric, no hint, Retryable false) is
// indistinguishable from transport noise and is refused retryably instead. A
// raw non-clierr probe error or a probe that returns no userID refuses
// retryably. A transient API-aggregation flap can momentarily drop the
// ext.cattle.io group from discovery and produce the same NOT_FOUND as a
// permanently absent endpoint, so the NOT_FOUND skip is a deliberate
// availability tradeoff — the guard is defense-in-depth; the destructive gates
// (--dry-run preview, --apply, --yes, cascade warning) remain the primary
// protection.
func guardSelfDelete(ctx context.Context, clients engine.ClusterClientFactory, in runtime.Input) error {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return nil
	}
	client, err := clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		// A classified clierr error (its own code and retryability already set)
		// passes through unchanged. An unclassified error — raw or a bare
		// CodeGeneric-no-hint-non-retryable clierr — is ambiguous; the guard
		// refuses retryably so an unverifiable identity costs a retry rather
		// than risking a lockout. Both paths share the same Classified predicate
		// as the probe branch below.
		return passClassifiedOrRefuse(err)
	}
	probe, err := client.Create(ctx, selfuser.GVR, "", selfuser.New(), false)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if clierr.IsNotFound(err) {
			return nil // ext.cattle.io absent on older Rancher — unsupported endpoint, skip
		}
		// Only classified errors pass through: a code other than Generic, a hint,
		// or Retryable true means mapErr (or the caller) already diagnosed the
		// fault. mapErr's catch-all default (CodeGeneric, no hint, Retryable false)
		// is indistinguishable from transport noise — treat it like a raw error and
		// refuse retryably so an unverifiable identity costs a retry rather than
		// risking a lockout.
		return passClassifiedOrRefuse(err)
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

// passClassifiedOrRefuse returns err unchanged when it is a classified clierr
// error — classified errors carry their own diagnosis and pass through; everything
// else — raw errors and the error map's unclassified catch-all — refuses retryably
// per the guard contract.
func passClassifiedOrRefuse(err error) error {
	var ce *clierr.Error
	if errors.As(err, &ce) && clierr.Classified(ce) {
		return err
	}
	return guardVerifyRefusal(err)
}
