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

// clientFactory implements engine.ClusterClientFactory for one invocation.
// Branch 2 adds once-only Effective resolution, Identity, and "" rejection;
// here it preserves existing per-miss behavior behind the new interface.
type clientFactory struct {
	dir   string
	store config.CredStore
	cache map[string]engine.ClusterClient
}

func newClientFactory(dir string, store config.CredStore) *clientFactory {
	return &clientFactory{dir: dir, store: store, cache: map[string]engine.ClusterClient{}}
}

// Cluster builds a creds-bound ClusterClient for a cluster proxy id at CALL
// time (network deferred). "local" targets the management cluster. Results are
// memoized per cluster id for the lifetime of the factory (one command
// invocation), so a command's Plan and Execute reuse a single client built from
// a single config + credential read — one identity, no re-read, and no window
// for a concurrent `login` to swap credentials between the two phases.
func (f *clientFactory) Cluster(_ context.Context, clusterID string) (engine.ClusterClient, error) {
	if c, ok := f.cache[clusterID]; ok {
		return c, nil
	}
	cfg, err := config.Load(config.FilePath(f.dir))
	if err != nil {
		return nil, err
	}
	eff, err := cfg.Effective(f.store)
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
	c, err := provider.NewClusterClient(rc)
	if err != nil {
		return nil, err
	}
	f.cache[clusterID] = c
	return c, nil
}

// AdaptEngine wires an engine command: it resolves the config dir/store per run
// (via resolveDeps), builds the run-time client factory, derives the resolver
// binding from Describe().Name, and runs the command through the pipeline.
func AdaptEngine(reg *runtime.Registry, make func(clients engine.ClusterClientFactory) runtime.Command) *ucli.Command {
	// Probe with a nil factory: engine constructors MUST NOT touch the factory in
	// Describe() (Describe is dir/store/client-independent). A command whose
	// Describe needs a client (e.g. a dynamic flag enum) would panic here — split
	// its construction instead of dereferencing the factory in Describe.
	probe := make(nil)
	d := probe.Describe()
	b := reg.Binding(d.Name)
	return &ucli.Command{
		Name:  leafName(d.Name),
		Usage: d.Usage,
		Flags: flagsFor(d),
		Action: func(ctx context.Context, c *ucli.Command) error {
			dir, store, err := resolveDeps(c)
			if err != nil {
				return exit(clierr.New(clierr.CodeGeneric, err.Error()))
			}
			cmd := make(newClientFactory(dir, store))
			if ce := runtime.Run(ctx, cmd, cmdInput{c}, buildIO(c), b); ce != nil {
				return exit(ce)
			}
			return nil
		},
	}
}
