package resources

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

type guardedDeleteCmd struct {
	inner    runtime.MutatingCommand
	guard    func(ctx context.Context, in runtime.Input) error
	warnings []string
}

// NewGuardedDelete wraps the generic delete for kinds whose deletion needs a
// bespoke refusal check and/or preview warnings. Plan runs inner.Plan FIRST
// (existence confirmed, resourceVersion captured, dry-run admission preview
// completed); on success the guard (may be nil) runs and may refuse by
// returning a non-nil error; warnings are appended to the Effect after both
// succeed. The tradeoff: a refused delete still performed the harmless
// server-side dry-run; refusal always precedes execute.
func NewGuardedDelete(k kinds.Kind, clients engine.ClusterClientFactory, guard func(ctx context.Context, in runtime.Input) error, warnings ...string) runtime.Command {
	inner, ok := NewDelete(k, clients).(runtime.MutatingCommand)
	if !ok {
		// Unreachable: NewDelete always returns a MutatingCommand.
		panic("NewDelete did not return a runtime.MutatingCommand")
	}
	return &guardedDeleteCmd{inner: inner, guard: guard, warnings: warnings}
}

func (c *guardedDeleteCmd) Describe() runtime.Descriptor {
	return c.inner.Describe()
}

func (c *guardedDeleteCmd) Plan(ctx context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	eff, exec, err := c.inner.Plan(ctx, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	if c.guard != nil {
		if err := c.guard(ctx, in); err != nil {
			return safety.Effect{}, nil, err
		}
	}
	eff.Warnings = append(eff.Warnings, c.warnings...)
	return eff, exec, nil
}

var _ runtime.MutatingCommand = (*guardedDeleteCmd)(nil)
