// Package managementtoken implements the overrides for the management-token
// (management.cattle.io/v3 Token) kind row: patch is refused outright (writes
// must go through Rancher's token lifecycle), and delete is guarded against
// removing the token backing the active CLI session.
package managementtoken

import (
	"context"
	"errors"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type refusePatchCmd struct {
	k kinds.Kind
}

// NewRefusePatch builds the patch override for the management-token row: a
// VALIDATION refusal — writes to management.cattle.io/v3 Token must go
// through Rancher's auth lifecycle (key generation, TTL clamp, identity
// derivation). Its Descriptor mirrors resources.NewPatch(k, ...) exactly so
// the row's schema/completion surface is unchanged; Plan refuses before any
// network call.
func NewRefusePatch(k kinds.Kind) runtime.Command {
	return &refusePatchCmd{k: k}
}

func (c *refusePatchCmd) Describe() runtime.Descriptor {
	return resources.NewPatch(c.k, nil, nil).Describe()
}

func (c *refusePatchCmd) Plan(_ context.Context, _ runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
		c.k.Name+": patch is not supported — writes go through Rancher's auth lifecycle (key generation, TTL clamp, identity derivation); the CLI does not bypass it").
		WithHint("use the Rancher API or UI to manage token fields")
}

var _ runtime.MutatingCommand = (*refusePatchCmd)(nil)

type deleteGuardCmd struct {
	inner   runtime.MutatingCommand
	clients engine.ClusterClientFactory
	namer   engine.SessionTokenNamer
	k       kinds.Kind
}

// NewDeleteGuard builds the delete override for the management-token row.
// It wraps resources.NewDelete's choreography (Route/Get/rv/dry-run/exec)
// unchanged, adding one check before delegation: if the target name matches
// the ID component of the current session's bearer token AND the token is not
// derived, refuse with a VALIDATION error. Derived tokens are deliberately
// deletable by their owning session (Rancher's /v3/tokens handler: refuses
// only when !currentAuthToken.GetIsDerived()). Deleting a non-derived active
// session token would cut off the CLI's own access mid-command.
//
// namer may be nil (zero runDeps for wireEntry validation); Plan skips the
// guard and delegates to the inner command in that case.
func NewDeleteGuard(k kinds.Kind, clients engine.ClusterClientFactory, namer engine.SessionTokenNamer) runtime.Command {
	inner, ok := resources.NewDelete(k, clients).(runtime.MutatingCommand)
	if !ok {
		panic("resources.NewDelete did not return a runtime.MutatingCommand")
	}
	return &deleteGuardCmd{inner: inner, clients: clients, namer: namer, k: k}
}

func (c *deleteGuardCmd) Describe() runtime.Descriptor {
	return c.inner.Describe()
}

func (c *deleteGuardCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	args := in.Args()
	if c.namer != nil && len(args) > 0 && args[0] != "" {
		activeID, err := c.namer.ActiveTokenID()
		if err != nil {
			return safety.Effect{}, nil, err
		}
		if activeID != "" && args[0] == activeID {
			// Name matches: single fetch via Route — the same fetched object
			// drives the isDerived check, the dry-run preview, and the exec
			// closure's resourceVersion precondition. No second GET can flip
			// isDerived between the guard decision and execution.
			clientCluster, ns, effectCluster, _, err := resources.Route(ctx, c.clients, c.k, in, true)
			if err != nil {
				return safety.Effect{}, nil, err
			}
			mgmt, err := c.clients.Cluster(ctx, clientCluster)
			if err != nil {
				return safety.Effect{}, nil, err
			}
			obj, err := mgmt.Get(ctx, c.k.GVR, ns, args[0])
			if err != nil {
				var ce *clierr.Error
				if errors.As(err, &ce) && ce.Code == clierr.CodeNotFound {
					// Target doesn't exist; let the generic delete produce its own NOT_FOUND.
					return c.inner.Plan(ctx, in)
				}
				return safety.Effect{}, nil, err
			}
			isDerived, _, _ := unstructured.NestedBool(obj.Object, "isDerived")
			if isDerived {
				// Derived token: deletion supported — build plan from the
				// already-fetched object so the exec's RV precondition is
				// bound to this read, not a second one.
				return resources.PlanFromFetched(ctx, mgmt, c.k, ns, args[0], obj, effectCluster)
			}
			return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
				"refusing to delete "+c.k.Name+" "+args[0]+": this token backs the current CLI session — deleting it would cut off the CLI's own access mid-command").
				WithHint("switch credentials first (re-login or set RANCHER_CLI_TOKEN), then delete this token from another session or the Rancher UI")
		}
	}
	return c.inner.Plan(ctx, in)
}

var _ runtime.MutatingCommand = (*deleteGuardCmd)(nil)
