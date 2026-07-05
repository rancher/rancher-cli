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
// Results are memoized per cluster id for the lifetime of the returned factory
// (one command invocation), so a command's Plan and Execute reuse a single Repo
// built from a single config + credential read — one identity, no re-read, and no
// window for a concurrent `login` to swap credentials between the two phases.
func newRepoFactory(dir string, store config.CredStore) engine.RepoFactory {
	cache := map[string]engine.Repo{}
	return func(clusterID string) (engine.Repo, error) {
		if r, ok := cache[clusterID]; ok {
			return r, nil
		}
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
		r, err := provider.NewRepo(rc)
		if err != nil {
			return nil, err
		}
		cache[clusterID] = r
		return r, nil
	}
}

// AdaptEngine wires an engine command: it resolves the config dir/store per run
// (via resolveDeps), builds the run-time Repo factory, injects the resolver's
// tier/stability, and runs the command through the pipeline.
func AdaptEngine(name string, reg *runtime.Registry, make func(newRepo engine.RepoFactory, tier, stability string) runtime.Command) *ucli.Command {
	tier, stability := reg.Binding(name)
	// Probe with a nil RepoFactory: engine constructors MUST NOT touch newRepo in
	// Describe() (Describe is dir/store/repo-independent). A command whose Describe
	// needs the Repo (e.g. a dynamic flag enum) would panic here — split its
	// construction instead of dereferencing newRepo in Describe.
	probe := make(nil, tier, stability)
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
