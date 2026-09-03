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
		c.k.Name+": patch is not supported — writes go through Rancher's auth lifecycle (key generation, TTL clamp, identity derivation); "+
			"the CLI does not bypass it; use the Rancher API or UI to manage token fields")
}

var _ runtime.MutatingCommand = (*refusePatchCmd)(nil)

type deleteGuardCmd struct {
	inner   runtime.MutatingCommand
	k       kinds.Kind
	clients engine.ClusterClientFactory
	namer   engine.SessionTokenNamer
}

// NewDeleteGuard builds the delete override for the management-token row.
// It is bespoke rather than using resources.NewGuardedDelete because the
// name-match path needs to issue exactly one GET for both the isDerived check
// and the exec's resourceVersion precondition — a second GET in the inner
// Plan is deliberately avoided to close the TOCTOU window between the guard
// decision and execution.
//
// When the target name matches the active session token:
//   - isDerived=true: returns PlanFromFetched from that single GET
//   - isDerived=false or absent: refuses with VALIDATION
//   - NOT_FOUND: falls through to the inner Plan's own NOT_FOUND
//
// When the target does not match (or namer is nil), the inner Plan runs with
// its standard Route/Get/rv/dry-run sequence.
//
// namer may be nil (zero runDeps for wireEntry validation); Plan skips the
// guard and delegates to the inner command in that case.
func NewDeleteGuard(k kinds.Kind, clients engine.ClusterClientFactory, namer engine.SessionTokenNamer) runtime.Command {
	inner, ok := resources.NewDelete(k, clients).(runtime.MutatingCommand)
	if !ok {
		// Unreachable: NewDelete always returns a MutatingCommand.
		panic("NewDelete did not return a runtime.MutatingCommand")
	}
	return &deleteGuardCmd{inner: inner, k: k, clients: clients, namer: namer}
}

func (c *deleteGuardCmd) Describe() runtime.Descriptor {
	return c.inner.Describe()
}

func (c *deleteGuardCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	if c.namer == nil {
		return c.inner.Plan(ctx, in)
	}
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return c.inner.Plan(ctx, in)
	}
	activeID, err := c.namer.ActiveTokenID()
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if activeID == "" || args[0] != activeID {
		return c.inner.Plan(ctx, in)
	}
	// Name matches the active session token: fetch the object once, decide
	// isDerived, and either refuse or return PlanFromFetched. The single GET
	// binds both the isDerived check and the exec's resourceVersion precondition
	// — a second GET is deliberately avoided to close the TOCTOU window between
	// the guard decision and execution.
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
		if ce, ok := errors.AsType[*clierr.Error](err); ok && ce.Code == clierr.CodeNotFound {
			// Target doesn't exist; fall through to the inner Plan's own NOT_FOUND.
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
		"refusing to delete "+c.k.Name+" "+args[0]+": this token backs the current CLI session — deleting it would cut off the CLI's own access mid-command; "+
			"switch credentials first (re-login or set RANCHER_CLI_TOKEN), then delete this token from another session or the Rancher UI")
}

var _ runtime.MutatingCommand = (*deleteGuardCmd)(nil)
