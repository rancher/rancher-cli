package cli

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/runtime"
	ucli "github.com/urfave/cli/v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var clustersGVR = schema.GroupVersionResource{Group: "management.cattle.io", Version: "v3", Resource: "clusters"}

// rootTimeout bounds the rootClient's HTTP requests. A package var (rather
// than a constant) so tests can shrink it — mirrors keyringProbeTimeout's
// test-shrinkable pattern in internal/config/credstore.go.
var rootTimeout = 30 * time.Second

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

	// clustersOnce fetches the management cluster LIST at most once per
	// invocation: ResolveCluster used to List on every unmemoized name, so
	// resolving N distinct display names (create kubeconfig --clusters a,b,c)
	// cost N network round trips — the name→id memo below only helped repeat
	// names. Every ResolveCluster call now matches against this one cached
	// list instead.
	clustersOnce sync.Once
	clusterList  *unstructured.UnstructuredList
	clustersErr  error

	mu       sync.Mutex
	cache    map[string]engine.ClusterClient
	resolved map[string]string // ResolveCluster memo: nameOrID → cluster id

	// rootOnce memoizes the root client (and its underlying httpx-built
	// http.Client) exactly once per invocation, same rationale as the
	// per-cluster cache above: one TLS-configured client, not one per call.
	rootOnce sync.Once
	root     engine.RootClient
	rootErr  error
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

func (f *clientFactory) ContextProject() (string, error) {
	eff, err := f.resolve()
	if err != nil {
		return "", err
	}
	return eff.Project, nil
}

// Root resolves the memoized engine.RootClient for this invocation, building
// the underlying httpx client at most once (RF-1: one server, one token, one
// TLS config per invocation).
func (f *clientFactory) Root() (engine.RootClient, error) {
	f.rootOnce.Do(func() {
		eff, err := f.resolve()
		if err != nil {
			f.rootErr = err
			return
		}
		caPEM, err := config.ResolveCA(eff.CA)
		if err != nil {
			f.rootErr = err
			return
		}
		hc, err := httpx.New(httpx.Options{CACert: caPEM, Timeout: rootTimeout})
		if err != nil {
			f.rootErr = err
			return
		}
		f.root = &rootClient{base: kube.BaseURL(eff.URL), token: eff.Token, hc: hc}
	})
	return f.root, f.rootErr
}

// rootClient implements engine.RootClient over httpx: one TLS-configured
// http.Client, bearer always attached (one policy — even for routes that
// are unauthenticated server-side), GET only.
type rootClient struct {
	base  string
	token string
	hc    *http.Client
}

func (r *rootClient) GetJSON(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+path, nil)
	if err != nil {
		return nil, err
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	req.Header.Set("Accept", "application/json")
	resp, body, err := httpx.Do(r.hc, req)
	if err != nil {
		return nil, clierr.New(clierr.CodeGeneric, fmt.Sprintf("rancher root GET %s: %v", path, err))
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return body, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, clierr.New(clierr.CodeAuth, fmt.Sprintf("rancher root GET %s: %s", path, resp.Status))
	case resp.StatusCode == http.StatusNotFound:
		return nil, clierr.New(clierr.CodeNotFound, fmt.Sprintf("rancher root GET %s: not found", path))
	default:
		return nil, clierr.New(clierr.CodeGeneric, fmt.Sprintf("rancher root GET %s: %s: %s", path, resp.Status, excerpt(body, 200)))
	}
}

// excerpt truncates b to at most n bytes for inclusion in an error message,
// appending "…" when truncated.
func excerpt(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (f *clientFactory) Identity() (string, error) {
	eff, err := f.resolve()
	if err != nil {
		return "", err
	}
	return eff.ServerName, nil
}

// clusters returns the management cluster LIST, fetched at most once per
// invocation regardless of how many distinct names ResolveCluster is asked
// to resolve. Concurrent callers block on the same sync.Once rather than
// racing duplicate Lists — unlike the per-name memo below, there is no
// per-name work to save by racing, so there is nothing "benign" to gain by
// not locking here.
func (f *clientFactory) clusters(ctx context.Context) (*unstructured.UnstructuredList, error) {
	f.clustersOnce.Do(func() {
		mgmt, err := f.Cluster(ctx, engine.LocalCluster)
		if err != nil {
			f.clustersErr = err
			return
		}
		f.clusterList, f.clustersErr = mgmt.List(ctx, clustersGVR, "", "")
	})
	return f.clusterList, f.clustersErr
}

// ResolveCluster accepts a cluster id or display name (agents pass names;
// rancher-ai-mcp accepts both — keep the semantics aligned). The matching now
// delegates to projects.MatchByIDOrDisplay — the same contract token minting
// and project/member resolution already share (this was the 4th copy).
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
	list, err := f.clusters(ctx)
	if err != nil {
		return "", err
	}
	var ids []string
	displayByID := map[string]string{}
	var cands []clierr.Candidate
	for i := range list.Items {
		id := list.Items[i].GetName()
		display, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "displayName")
		ids = append(ids, id)
		displayByID[id] = display
		cands = append(cands, clierr.Candidate{ID: id, Name: display})
	}
	matched, idFound, displayMatchCount := projects.MatchByIDOrDisplay(nameOrID, ids, func(id string) string { return displayByID[id] })
	switch len(matched) {
	case 1:
		f.mu.Lock()
		f.resolved[nameOrID] = matched[0]
		f.mu.Unlock()
		return matched[0], nil
	case 0:
		return "", clierr.New(clierr.CodeNotFound, "cluster not found: "+nameOrID).
			WithCandidates(cands).WithHint("pass a cluster id or display name")
	default:
		msg := fmt.Sprintf("%d clusters have display name %q", displayMatchCount, nameOrID)
		if idFound {
			msg = fmt.Sprintf("%q matches both a cluster id and a different cluster's display name", nameOrID)
		}
		return "", clierr.New(clierr.CodeAmbiguousSelection, msg).
			WithCandidates(cands).WithHint("pass the cluster id")
	}
}

// runDeps bundles everything a command constructor might need for one run: the
// per-run cluster client factory, the file source for -f input, and the
// resolved config dir/store. Each entry's constructor takes what it needs and
// ignores the rest, so a command is one entry row — no per-shape adapter or
// wiring switch.
type runDeps struct {
	clients engine.ClusterClientFactory
	roots   engine.RootClientFactory
	src     engine.FileSource
	dir     string
	store   config.CredStore
}

// adaptEntry wires one declarative entry through the single run path: it
// resolves the config dir/store, builds the client factory and file source,
// stamps server identity, derives the resolver binding, and runs the command
// through the pipeline. The registry probe passes a zero runDeps — a
// constructor MUST NOT touch its deps in Describe() (Describe is
// dir/store/client-independent); one that does would panic here rather than in
// production.
func adaptEntry(reg *runtime.Registry, make func(runDeps) runtime.Command) *ucli.Command {
	// probe is the SAME zero-runDeps instance used for both Describe() and the
	// ShellCompleter type-assert below — its dir field is permanently "". A
	// completer wired here is later called with this exact probe at real
	// TAB time too, so CompleteArgs must use only its dir PARAMETER, never
	// receiver state (a completer reading its own dir field would pass a unit
	// test built with a real dir and return empty forever at real TAB time).
	probe := make(runDeps{})
	d := probe.Describe()
	b := reg.Binding(d.Name)
	cmd := &ucli.Command{
		Name:         leafName(d.Name),
		Aliases:      d.Aliases,
		Usage:        d.Usage,
		ArgsUsage:    argsUsage(d.Args),
		Flags:        flagsFor(d),
		OnUsageError: onUsageError,
		Action: func(ctx context.Context, c *ucli.Command) error {
			dir, store, err := resolveDeps(c)
			if err != nil {
				return exit(clierr.New(clierr.CodeGeneric, err.Error()))
			}
			f := newClientFactory(dir, store)
			io := buildIO(c)
			io.ServerIdentity = f.Identity
			cmd := make(runDeps{clients: f, roots: f, src: newFileSource(io.In), dir: dir, store: store})
			if ce := runtime.Run(ctx, cmd, cmdInput{c}, io, b); ce != nil {
				return exit(ce)
			}
			return nil
		},
	}
	installShellComplete(probe, cmd)
	return cmd
}
