package cli

import (
	"context"
	"sync"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	ucli "github.com/urfave/cli/v3"
)

// clientFactory implements engine.ClusterClientFactory for one invocation.
// Config + credentials resolve exactly once (RF-1): one server, one token,
// one context snapshot per invocation — a concurrent `auth login` cannot
// swap identity between Plan and Execute.
type clientFactory struct {
	dir   string
	store config.CredStore

	once       sync.Once
	eff        config.Effective
	resolveErr error

	mu    sync.Mutex
	cache map[string]engine.ClusterClient
}

func newClientFactory(dir string, store config.CredStore) *clientFactory {
	return &clientFactory{dir: dir, store: store, cache: map[string]engine.ClusterClient{}}
}

func (f *clientFactory) resolve() (config.Effective, error) {
	f.once.Do(func() {
		cfg, err := config.Load(config.FilePath(f.dir))
		if err != nil {
			f.resolveErr = err
			return
		}
		f.eff, f.resolveErr = cfg.Effective(f.store)
	})
	return f.eff, f.resolveErr
}

func (f *clientFactory) Cluster(_ context.Context, clusterID string) (engine.ClusterClient, error) {
	if clusterID == "" {
		return nil, clierr.New(clierr.CodeValidation,
			"cluster id must not be empty").
			WithHint("pass engine.LocalCluster or an explicit cluster id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.cache[clusterID]; ok {
		return c, nil
	}
	eff, err := f.resolve()
	if err != nil {
		return nil, err
	}
	eff.Cluster = clusterID // eff is a copy; the resolved snapshot is not mutated
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

func (f *clientFactory) ContextCluster() (string, error) {
	eff, err := f.resolve()
	if err != nil {
		return "", err
	}
	return eff.Cluster, nil
}

func (f *clientFactory) Identity() (string, error) {
	eff, err := f.resolve()
	if err != nil {
		return "", err
	}
	return eff.ServerName, nil
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
