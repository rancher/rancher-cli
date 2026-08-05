package resources

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

type guardedDeleteCmd struct {
	inner runtime.MutatingCommand
	guard func(ctx context.Context, in runtime.Input) error
	// prePlan controls when the guard runs relative to the inner Plan.
	//
	// false (PostPlan, the default via NewGuardedDelete): the inner Plan
	// runs first — existence confirmed, resourceVersion captured, dry-run
	// admission preview completed — then the guard may refuse. A nonexistent
	// target fails NOT_FOUND before the guard fires. Tradeoff: a refused
	// delete still performed the harmless server-side dry-run.
	//
	// true (PrePlan, via NewGuardedDeletePrePlan): the guard runs before the
	// inner Plan's server-side dry-run. Required when the server itself would
	// refuse the dry-run with a rawer version of the guard's own message
	// (e.g. the self-delete webhook on users), so the curated refusal always
	// wins over raw webhook text. Cost: the guard fires — including any
	// identity probe — even for nonexistent targets (a mistyped name triggers
	// one probe before the 404). For user deletion the probe is a cheap
	// read-equivalent call; the accepted tradeoff is documented at
	// NewGuardedDeletePrePlan. A third timing variant is the signal to move
	// that kind bespoke rather than grow this flag.
	prePlan  bool
	warnings []string
}

// NewGuardedDelete wraps the generic delete for kinds whose deletion needs a
// bespoke refusal check and/or preview warnings. Plan runs inner.Plan FIRST
// (PostPlan timing; see the prePlan field doc for the full tradeoff). On
// success the guard (may be nil) runs and may refuse by returning a non-nil
// error; warnings are appended to the Effect after both succeed.
//
// Use NewGuardedDeletePrePlan when the server itself would deny the inner
// Plan's dry-run before the guard can fire.
func NewGuardedDelete(k kinds.Kind, clients engine.ClusterClientFactory, guard func(ctx context.Context, in runtime.Input) error, warnings ...string) runtime.Command {
	return newGuardedDelete(k, clients, false, guard, warnings...)
}

// NewGuardedDeletePrePlan wraps the generic delete with the guard running
// before the inner Plan's server-side dry-run (prePlan=true). Use when the
// server itself would refuse the dry-run with a rawer version of the guard's
// own message — the curated refusal must win. See the prePlan field doc for
// the complete every-invocation-probe cost and accepted tradeoff.
func NewGuardedDeletePrePlan(k kinds.Kind, clients engine.ClusterClientFactory, guard func(ctx context.Context, in runtime.Input) error, warnings ...string) runtime.Command {
	return newGuardedDelete(k, clients, true, guard, warnings...)
}

func newGuardedDelete(k kinds.Kind, clients engine.ClusterClientFactory, prePlan bool, guard func(ctx context.Context, in runtime.Input) error, warnings ...string) runtime.Command {
	inner, ok := NewDelete(k, clients).(runtime.MutatingCommand)
	if !ok {
		// Unreachable: NewDelete always returns a MutatingCommand.
		panic("NewDelete did not return a runtime.MutatingCommand")
	}
	return &guardedDeleteCmd{inner: inner, guard: guard, prePlan: prePlan, warnings: warnings}
}

func (c *guardedDeleteCmd) Describe() runtime.Descriptor {
	return c.inner.Describe()
}

func (c *guardedDeleteCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	if c.prePlan && c.guard != nil {
		if err := c.guard(ctx, in); err != nil {
			return safety.Effect{}, nil, err
		}
	}
	eff, exec, err := c.inner.Plan(ctx, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if !c.prePlan && c.guard != nil {
		if err := c.guard(ctx, in); err != nil {
			return safety.Effect{}, nil, err
		}
	}
	eff.Warnings = append(eff.Warnings, c.warnings...)
	return eff, exec, nil
}

var _ runtime.MutatingCommand = (*guardedDeleteCmd)(nil)
