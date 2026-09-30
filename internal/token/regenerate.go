package token

import (
	"errors"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
)

// RegenerateSteps are the commands that write the kubeconfig for cluster
// again from entry, the server entry it was written from: select that
// entry, then write the kubeconfig. The selection is always named, since
// the text is read later, when another server may be current, and so is
// what it does to the directory. rancher opens each command: clierr.Rancher
// for text an invocation prints, or rancher and its --config for text kept
// in a file. An empty entry or cluster names the placeholder.
func RegenerateSteps(rancher, entry, cluster string) string {
	e, c := "<entry>", "<id>"
	if entry != "" {
		e = clierr.ShellQuote(entry)
	}
	if cluster != "" {
		c = clierr.ShellQuote(cluster)
	}
	return rancher + " config use-server " + e + " (this switches the directory's current server), then " +
		rancher + " kubeconfig --cluster " + c
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
	steps := RegenerateSteps(clierr.Rancher, entryOfName(cfg, applyDefaultScheme(server), name, clusterID, false), clusterID)
	return ce.WithMessage(ce.Message + "; if a kubeconfig's exec entry sets it, regenerate the kubeconfig with " +
		config.EnvCredentialStore + " unset: " + steps)
}
