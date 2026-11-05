package resources

import (
	"context"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
)

type refusePatchCmd struct {
	k kinds.Kind
}

// NewRefusePatch builds the patch command of a row that sets NoPatchReason: a
// VALIDATION refusal that comes before any request. Its descriptor is the
// generic patch's, so the flags and aliases of the row do not change, with a
// usage line and a description that say the verb is always refused.
func NewRefusePatch(k kinds.Kind) runtime.Command {
	return &refusePatchCmd{k: k}
}

func (c *refusePatchCmd) Describe() runtime.Descriptor {
	d := NewPatch(c.k, nil, nil).Describe()
	d.Usage = "Refused: " + c.k.Name + " cannot be patched"
	d.Desc = "Always refused before any request: " + c.k.NoPatchReason + "."
	return d
}

func (c *refusePatchCmd) Plan(_ context.Context, in runtime.Input) (safety.Effect, runtime.ExecuteFunc, error) {
	return safety.Effect{}, nil, clierr.New(clierr.CodeValidation,
		c.k.Name+": patch is not supported - "+c.k.NoPatchReason+"; read it with "+clierr.Rancher+" get"+c.readFlags(in))
}

// readFlags writes the name of the row and the routing flags the read of it
// needs, taking the values the caller typed so the command works as printed.
// A downstream row with no typed --cluster names the flag with a placeholder.
func (c *refusePatchCmd) readFlags(in runtime.Input) string {
	var flags string
	if c.k.Scope == kinds.Downstream {
		cluster := "<cluster>"
		if typed := in.String(FlagCluster); typed != "" {
			cluster = clierr.ShellQuote(typed)
		}
		flags += " --cluster " + cluster
	}
	if c.k.Policy == kinds.NSPlain {
		if ns := in.String(FlagNamespace); ns != "" {
			flags += " --namespace " + clierr.ShellQuote(ns)
		}
	}
	return clierr.ShellQuoteArgs(flags, c.k.Name)
}

var _ runtime.MutatingCommand = (*refusePatchCmd)(nil)
