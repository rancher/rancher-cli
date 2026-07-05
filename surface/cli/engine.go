package cli

import (
	"context"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	ucli "github.com/urfave/cli/v3"
)

// newRepoFactory returns a factory that builds a creds-bound Repo for a cluster
// proxy id at CALL time (network deferred). "local" targets the management cluster.
func newRepoFactory(dir string, store config.CredStore) engine.RepoFactory {
	return func(clusterID string) (engine.Repo, error) {
		cfg, err := config.Load(config.FilePath(dir))
		if err != nil {
			return nil, err
		}
		eff, err := cfg.Effective(store)
		if err != nil {
			return nil, err
		}
		if clusterID != "" {
			eff.Cluster = clusterID
		}
		rc, err := kube.RESTConfig(eff)
		if err != nil {
			return nil, err
		}
		return provider.NewRepo(rc)
	}
}

// AdaptEngine wires an engine command: it resolves the config dir/store per run
// (via resolveDeps), builds the run-time Repo factory, injects the resolver's
// tier/stability, and runs the command through the pipeline.
func AdaptEngine(name string, reg *runtime.Registry, make func(newRepo engine.RepoFactory, tier, stability string) runtime.Command) *ucli.Command {
	tier, stability := reg.Binding(name)
	probe := make(nil, tier, stability) // Describe is dir/store/repo-independent
	d := probe.Describe()
	return &ucli.Command{
		Name:  leafName(d.Name),
		Usage: d.Usage,
		Flags: flagsFor(d),
		Action: func(ctx context.Context, c *ucli.Command) error {
			dir, store, err := resolveDeps(c)
			if err != nil {
				return exit(clierr.New(clierr.CodeGeneric, err.Error()))
			}
			cmd := make(newRepoFactory(dir, store), tier, stability)
			if ce := runtime.Run(ctx, cmd, cmdInput{c}, buildIO(c)); ce != nil {
				return exit(ce)
			}
			return nil
		},
	}
}
