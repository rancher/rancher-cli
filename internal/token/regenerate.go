package token

import (
	"errors"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// RegenerateNote is what the text after RegenerateSteps says of the first
// step: the selection is always named, since the text is read later, when
// another server may be current.
const RegenerateNote = "; selecting the entry switches the directory's current server"

// RegenerateSteps are the commands that write the kubeconfig for cluster
// again from entry, the server entry it was written from: select that
// entry, then write the kubeconfig. start opens each command: clierr.NewCommand
// for text an invocation prints, or a closure over clierr.FileCommandIn for
// text kept in a file. An empty entry or cluster names the placeholder. The
// steps are left out whole when a value in them cannot be printed.
func RegenerateSteps(start func(clierr.CommandWord) clierr.Command, entry, cluster string) clierr.Sequence {
	use, kubeconfig := start("config use-server"), start("kubeconfig")
	if entry == "" {
		use = use.Placeholder("<entry>")
	} else {
		use = use.Arg(entry)
	}
	if cluster == "" {
		kubeconfig = kubeconfig.FlagText(" --cluster <id>")
	} else {
		kubeconfig = kubeconfig.FlagValue("--cluster", cluster)
	}
	return clierr.NewSequence().Then("", use).Then("then ", kubeconfig)
}

// withStoreConflictSteps adds to the refusal of a directory whose declared
// store the variable contradicts what to do when the variable comes from
// the exec entry: a kubeconfig written while the directory declared
// nothing records the variable, and the operator cannot unset it there.
// The hint (unset the variable) stays, for the shell's own.
func (t tokenCmd) withStoreConflictSteps(err error, server, name, clusterID string) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok || !errors.Is(err, config.ErrCredentialStoreConflict) {
		return err
	}
	cfg, lerr := config.Load(config.FilePath(t.dir))
	if lerr != nil {
		cfg = nil
	}
	steps := RegenerateSteps(clierr.NewCommand, entryOfName(cfg, ApplyDefaultScheme(server), name, clusterID, false), clusterID)
	return ce.WithMessage(clierr.Text(ce.Message+"; if a kubeconfig's exec entry sets it, regenerate the kubeconfig with "+config.EnvCredentialStore+" unset",
		clierr.Lead(": "), steps, RegenerateNote))
}
