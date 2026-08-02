package resources

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

// GuardTiming decides when a guarded delete's guard runs relative to
// the inner Plan. PostPlan (the default) lets a nonexistent target
// fail NOT_FOUND before any guard work — right for guards that only
// need the input. PrePlan runs the guard before the inner Plan's
// server dry-run — required when the server itself would refuse the
// dry-run with a rawer version of the guard's own message (the
// self-delete webhook), at the cost of the guard's probe firing for
// nonexistent targets too.
type GuardTiming int

const (
	// PostPlan runs the guard after the inner Plan's server-side dry-run.
	// A nonexistent target fails NOT_FOUND before the guard runs.
	PostPlan GuardTiming = iota
	// PrePlan runs the guard before the inner Plan's server-side dry-run.
	// A mistyped target name fires the guard's probe before the 404.
	PrePlan
)

type guardedDeleteCmd struct {
	inner    runtime.MutatingCommand
	guard    func(ctx context.Context, in runtime.Input) error
	timing   GuardTiming
	warnings []string
}

// NewGuardedDelete wraps the generic delete for kinds whose deletion needs a
// bespoke refusal check and/or preview warnings. Plan runs inner.Plan FIRST
// (existence confirmed, resourceVersion captured, dry-run admission preview
// completed); on success the guard (may be nil) runs and may refuse by
// returning a non-nil error; warnings are appended to the Effect after both
// succeed. The tradeoff: a refused delete still performed the harmless
// server-side dry-run; refusal always precedes execute.
//
// Use NewGuardedDeletePrePlan when the server itself would deny the inner
// Plan's dry-run before the guard can fire (e.g. an admission webhook that
// refuses the same operation the guard is protecting against).
func NewGuardedDelete(k kinds.Kind, clients engine.ClusterClientFactory, guard func(ctx context.Context, in runtime.Input) error, warnings ...string) runtime.Command {
	return newGuardedDelete(k, clients, PostPlan, guard, warnings...)
}

// NewGuardedDeletePrePlan wraps the generic delete with the guard running
// before the inner Plan's server-side dry-run (PrePlan timing). Use when the
// server itself would refuse the dry-run with a rawer version of the guard's
// own message — the curated refusal must win. Accepted tradeoff: the guard
// fires (including any probe) even for nonexistent targets.
func NewGuardedDeletePrePlan(k kinds.Kind, clients engine.ClusterClientFactory, guard func(ctx context.Context, in runtime.Input) error, warnings ...string) runtime.Command {
	return newGuardedDelete(k, clients, PrePlan, guard, warnings...)
}

func newGuardedDelete(k kinds.Kind, clients engine.ClusterClientFactory, timing GuardTiming, guard func(ctx context.Context, in runtime.Input) error, warnings ...string) runtime.Command {
	inner, ok := NewDelete(k, clients).(runtime.MutatingCommand)
	if !ok {
		// Unreachable: NewDelete always returns a MutatingCommand.
		panic("NewDelete did not return a runtime.MutatingCommand")
	}
	return &guardedDeleteCmd{inner: inner, guard: guard, timing: timing, warnings: warnings}
}

func (c *guardedDeleteCmd) Describe() runtime.Descriptor {
	return c.inner.Describe()
}

func (c *guardedDeleteCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	if c.timing == PrePlan && c.guard != nil {
		if err := c.guard(ctx, in); err != nil {
			return safety.Effect{}, nil, err
		}
	}
	eff, exec, err := c.inner.Plan(ctx, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if c.timing == PostPlan && c.guard != nil {
		if err := c.guard(ctx, in); err != nil {
			return safety.Effect{}, nil, err
		}
	}
	eff.Warnings = append(eff.Warnings, c.warnings...)
	return eff, exec, nil
}

var _ runtime.MutatingCommand = (*guardedDeleteCmd)(nil)
