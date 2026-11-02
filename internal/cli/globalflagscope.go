package cli

import (
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/runtime"
	ucli "github.com/urfave/cli/v3"
)

// commandAcceptsGlobalFlag reports whether the command, or a command group,
// accepts the global flag name (its long name without the dashes). The
// schema manifest's appliesTo for the flag decides, with the command type
// and, for --project, whether the command takes it. A command group accepts
// a flag when any command under it does, hidden commands included. A flag
// the manifest does not list (help, version) is accepted everywhere, and a
// command with no descriptor accepts every flag: a command that cannot be
// classified loses nothing from its offer.
func commandAcceptsGlobalFlag(cmd *ucli.Command, name string) bool {
	i := slices.IndexFunc(runtime.GlobalFlags(), func(gf runtime.GlobalFlag) bool {
		_, long, found := strings.CutLast(gf.Name, "--")
		return found && long == name
	})
	if i < 0 {
		return true
	}
	return commandAcceptsAppliesTo(cmd, runtime.GlobalFlags()[i].AppliesTo)
}

func commandAcceptsAppliesTo(cmd *ucli.Command, appliesTo string) bool {
	if len(cmd.Commands) > 0 {
		return slices.ContainsFunc(cmd.Commands, func(sub *ucli.Command) bool {
			return commandAcceptsAppliesTo(sub, appliesTo)
		})
	}
	d, ok := commandDescriptor(cmd)
	if !ok {
		return true
	}
	return appliesToAccepts(appliesTo, d.CommandType, runtime.TakesProject(d))
}

// appliesToAccepts reports whether a command of type t accepts a global flag
// with the manifest's appliesTo value: "mutating" is the mutating commands,
// "server" the commands that read or change server resources (the pipeline
// refuses the flag on local commands), "declared" the commands that take the
// flag themselves and "all" every command. A value it does not know is
// accepted.
func appliesToAccepts(appliesTo string, t runtime.CommandType, takesProject bool) bool {
	switch appliesTo {
	case "mutating":
		return t == runtime.Mutating
	case "server":
		return t == runtime.ReadOnly || t == runtime.Mutating
	case "declared":
		return takesProject
	}
	return true
}
