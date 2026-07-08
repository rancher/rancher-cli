package cli

import (
	"context"
	"fmt"
	"sync"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime"
	ucli "github.com/urfave/cli/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var clustersGVR = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusters"}

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

	mu       sync.Mutex
	cache    map[string]engine.ClusterClient
	resolved map[string]string // ResolveCluster memo: nameOrID → cluster id
}

func newClientFactory(dir string, store config.CredStore) *clientFactory {
	return &clientFactory{
		dir:      dir,
		store:    store,
		cache:    map[string]engine.ClusterClient{},
		resolved: map[string]string{},
	}
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
	// Resolve config+credentials before locking: sync.Once serializes its own
	// first run, so the mutex only needs to guard the cache map — not the
	// one-time config file read.
	eff, err := f.resolve()
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.cache[clusterID]; ok {
		return c, nil
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

// ResolveCluster accepts a cluster id or display name (agents pass names;
// rancher-ai-mcp accepts both — keep the semantics aligned).
func (f *clientFactory) ResolveCluster(ctx context.Context, nameOrID string) (string, error) {
	if nameOrID == "" || nameOrID == engine.LocalCluster {
		return engine.LocalCluster, nil
	}
	f.mu.Lock()
	if id, ok := f.resolved[nameOrID]; ok {
		f.mu.Unlock()
		return id, nil
	}
	f.mu.Unlock()
	mgmt, err := f.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return "", err
	}
	list, err := mgmt.List(ctx, clustersGVR, "")
	if err != nil {
		return "", err
	}
	var matches []string
	var cands []clierr.Candidate
	for i := range list.Items {
		id := list.Items[i].GetName()
		display, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "displayName")
		cands = append(cands, clierr.Candidate{ID: id, Name: display})
		if id == nameOrID {
			matches = []string{id}
			break
		}
		if display == nameOrID {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 1:
		f.mu.Lock()
		f.resolved[nameOrID] = matches[0]
		f.mu.Unlock()
		return matches[0], nil
	case 0:
		return "", clierr.New(clierr.CodeNotFound, "cluster not found: "+nameOrID).
			WithCandidates(cands).WithHint("pass a cluster id or display name")
	default:
		return "", clierr.New(clierr.CodeAmbiguousSelection,
			fmt.Sprintf("%d clusters have display name %q", len(matches), nameOrID)).
			WithCandidates(cands).WithHint("pass the cluster id")
	}
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
			f := newClientFactory(dir, store)
			cmd := make(f)
			io := buildIO(c)
			io.ServerIdentity = f.Identity
			if ce := runtime.Run(ctx, cmd, cmdInput{c}, io, b); ce != nil {
				return exit(ce)
			}
			return nil
		},
	}
}
