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
// generic patch's, except for the usage line and the description, which say
// the verb is always refused.
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
		clierr.Text(c.k.Name+": patch is not supported - "+c.k.NoPatchReason, clierr.Lead("; read it with "),
			clierr.NewCommand("get").Arg(c.k.Name).With(c.readFlags(in))))
}

// readFlags returns the flags of the read command: --cluster for a downstream
// row, and --namespace for a row whose namespace policy is NSPlain or
// NSWorkspace (a Fleet workspace), when the user typed one. A downstream row
// with no typed --cluster gets <cluster> as a placeholder.
func (c *refusePatchCmd) readFlags(in runtime.Input) clierr.CommandOption {
	return func(cmd clierr.Command) clierr.Command {
		if c.k.Scope == kinds.Downstream {
			if typed := in.String(FlagCluster); typed != "" {
				cmd = cmd.FlagValue("--cluster", typed)
			} else {
				cmd = cmd.FlagText(" --cluster <cluster>")
			}
		}
		if c.k.Policy == kinds.NSPlain || c.k.Policy == kinds.NSWorkspace {
			if ns := in.String(FlagNamespace); ns != "" {
				cmd = cmd.FlagValue("--namespace", ns)
			}
		}
		return cmd
	}
}

var _ runtime.MutatingCommand = (*refusePatchCmd)(nil)
