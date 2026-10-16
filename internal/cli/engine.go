package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/client/httpx"
	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/client/provider"
	"github.com/rancher/rancher-cli/internal/client/tlstrust"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	ucli "github.com/urfave/cli/v3"
)

// rootTimeout bounds the rootClient's HTTP requests. A package var (rather
// than a constant) so tests can shrink it - mirrors keyringProbeTimeout's
// test-shrinkable pattern in internal/config/credstore.go.
var rootTimeout = 30 * time.Second

// clientFactory implements engine.ClusterClientFactory for one invocation.
// Config + credentials resolve exactly once: one server, one token,
// one context snapshot per invocation - a concurrent `auth login` cannot
// swap identity between Plan and Execute.
type clientFactory struct {
	dir   string
	store config.CredStore

	once       sync.Once
	eff        config.Effective
	resolveErr error

	// clustersOnce reads the clusters the caller can read at most once per
	// invocation, so resolving N distinct display names (create kubeconfig
	// --clusters a,b,c) costs one collection read, not N.
	clustersOnce sync.Once
	clusterList  []steve.Cluster
	clustersErr  error

	mu       sync.Mutex
	cache    map[string]engine.ClusterClient
	resolved map[string]string // ResolveCluster memo: nameOrID → cluster id
	notices  []string          // ResolveCluster warnings, each recorded once

	// rootOnce memoizes the root client (and its underlying httpx-built
	// http.Client) exactly once per invocation, same rationale as the
	// per-cluster cache above: one TLS-configured client, not one per call.
	rootOnce sync.Once
	root     engine.RootClient
	rootErr  error

	// serverInfoOnce memoizes the /rancherversion version+edition read
	// exactly once per invocation (f is itself the per-invocation instance):
	// the capability detector and any future consumer sharing this factory
	// issue at most one GET regardless of how many times ServerInfo is
	// called.
	serverInfoOnce sync.Once
	serverInfo     provider.ServerInfo
	serverInfoErr  error
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
		return nil, clierr.New(clierr.CodeValidation, "cluster id must not be empty")
	}
	// Resolve config+credentials before locking: sync.Once serializes its own
	// first run, so the mutex only needs to guard the cache map - not the
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
	rc, err := kube.RESTConfig(eff, clusterID)
	if err != nil {
		return nil, err
	}
	c, err := provider.NewClusterClient(rc, clusterID)
	if err != nil {
		return nil, err
	}
	f.cache[clusterID] = c
	return c, nil
}

// ImpersonatedCluster is Cluster acting as another user; the memo key
// carries the identity so two subjects never share a client.
func (f *clientFactory) ImpersonatedCluster(_ context.Context, clusterID string, as engine.Impersonation) (engine.ClusterClient, error) {
	if clusterID == "" {
		return nil, clierr.New(clierr.CodeValidation, "cluster id must not be empty")
	}
	if as.User == "" {
		return nil, clierr.New(clierr.CodeValidation, "impersonated user must not be empty")
	}
	eff, err := f.resolve()
	if err != nil {
		return nil, err
	}
	// A cluster id cannot contain a NUL byte, so an impersonated key never
	// collides with a plain one.
	key := clusterID + "\x00as\x00" + as.User
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.cache[key]; ok {
		return c, nil
	}
	rc, err := kube.RESTConfigImpersonating(eff, clusterID, as.User)
	if err != nil {
		return nil, err
	}
	c, err := provider.NewClusterClient(rc, clusterID)
	if err != nil {
		return nil, err
	}
	f.cache[key] = c
	return c, nil
}

// Root resolves the memoized engine.RootClient for this invocation, building
// the underlying httpx client at most once (one server, one token, one
// TLS config per invocation).
func (f *clientFactory) Root() (engine.RootClient, error) {
	f.rootOnce.Do(func() {
		eff, err := f.resolve()
		if err != nil {
			f.rootErr = err
			return
		}
		if err := kube.RequireLogin(eff); err != nil {
			f.rootErr = err
			return
		}
		caPEM, err := config.ResolveCA(eff.CA)
		if err != nil {
			f.rootErr = err
			return
		}
		hc, err := httpx.New(httpx.Options{
			CACert:        caPEM,
			Timeout:       rootTimeout,
			WrapTransport: tlstrust.WrapTransportFor(eff.URL, eff.TLSRemedy),
		})
		if err != nil {
			f.rootErr = err
			return
		}
		f.root = &rootClient{base: kube.BaseURL(eff.URL), token: eff.Token, hc: hc}
	})
	return f.root, f.rootErr
}

// rootClient implements engine.RootClient over httpx: one TLS-configured
// http.Client, bearer always attached (one policy - even for routes that
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
		if ce := tlstrust.TypedError(err); ce != nil {
			return nil, ce
		}
		return nil, clierr.New(clierr.CodeGeneric, fmt.Sprintf("rancher root GET %s: %v", path, err))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return body, nil
	}
	return nil, clierr.FromResponse("rancher root GET "+path, http.MethodGet, resp.StatusCode, body, &clierr.Request{})
}

// ServerInfo resolves the memoized /rancherversion version+edition for this
// invocation, fetching at most once (mirroring Root's rootOnce): the
// unauthenticated config.Effective-built HTTP path RancherVersion uses
// today, never engine.RootClientFactory.
func (f *clientFactory) ServerInfo(ctx context.Context) (provider.ServerInfo, error) {
	f.serverInfoOnce.Do(func() {
		eff, err := f.resolve()
		if err != nil {
			f.serverInfoErr = err
			return
		}
		f.serverInfo, f.serverInfoErr = provider.GetServerInfo(ctx, eff)
	})
	return f.serverInfo, f.serverInfoErr
}

func (f *clientFactory) Identity() (string, error) {
	eff, err := f.resolve()
	if err != nil {
		return "", err
	}
	return eff.ServerName, nil
}

// ServerBaseURL implements engine.ServerBaseURLProvider: the same
// kube.BaseURL(eff.URL) value Root() bakes into rootClient.base - scheme
// preserved, no trailing slash, honoring a RANCHER_CLI_URL override or an
// http development server. Deliberately NOT eff.ServerName (Identity's
// display-only value): that name is cleared under a URL override and
// carries no scheme.
func (f *clientFactory) ServerBaseURL() (string, error) {
	eff, err := f.resolve()
	if err != nil {
		return "", err
	}
	return kube.BaseURL(eff.URL), nil
}

// ActiveTokenID implements engine.SessionTokenNamer. It returns the name
// component of the current bearer token. For id:secret bearers it is the id
// half. For Rancher-issued JWTs (three dot-separated base64url segments, no
// colon) the payload's "token" claim is used when "token_kind" is "v3"; for
// ext-kind JWTs the backing token is an ext.cattle.io Token (a different
// resource), so "" is returned and the guard skips. Unparseable bearers also
// return "" - the guard is best-effort convenience, not an authorization
// decision; the server enforces authz on the delete itself.
// Returns "" when no credential is stored or resolved. The secret half is
// never exposed.
func (f *clientFactory) ActiveTokenID() (string, error) {
	eff, err := f.resolve()
	if err != nil {
		return "", err
	}
	return jwtTokenID(eff.Token), nil
}

// jwtTokenID extracts the backing token name from a bearer credential.
// For id:secret bearers it returns the id half. For Rancher-issued JWTs it
// decodes the payload (base64url, no padding) and reads the "token" claim
// when "token_kind" is "v3". Returns "" for ext-kind claims, malformed input,
// or empty input.
//
// NO signature verification is performed: this is a local footgun guard
// resolving the caller's own identity hint, not an authorization decision -
// the server enforces authz on the delete itself.
func jwtTokenID(bearer string) string {
	if id, _, ok := strings.Cut(bearer, ":"); ok {
		return id
	}
	parts := strings.Split(bearer, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Token string `json:"token"`
		Kind  string `json:"token_kind"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	if claims.Kind != "v3" {
		return ""
	}
	return claims.Token
}

// clusters returns the clusters the caller can read, read through steve at
// most once per invocation regardless of how many distinct names
// ResolveCluster is asked to resolve. Concurrent callers block on the same
// sync.Once rather than racing duplicate reads.
func (f *clientFactory) clusters(ctx context.Context) (steve.Getter, []steve.Cluster, error) {
	root, err := f.Root()
	if err != nil {
		return nil, nil, err
	}
	get := steve.RootGetter(root)
	f.clustersOnce.Do(func() {
		f.clusterList, f.clustersErr = steve.Clusters(ctx, get)
	})
	return get, f.clusterList, f.clustersErr
}

// ResolveCluster accepts a cluster id or display name among the clusters
// the caller can read (agents pass names; rancher-ai-mcp accepts both -
// keep the semantics aligned). "" and "local" are the local cluster and
// send no request. An id that is also another cluster's display name wins,
// with a warning the factory hands to the pipeline as a notice.
func (f *clientFactory) ResolveCluster(ctx context.Context, nameOrID string) (string, error) {
	return f.resolveCluster(ctx, nameOrID, "--cluster")
}

// ResolveClusterArgument implements engine.ClusterArgumentResolver: the
// warning of a positional value or a list entry names no flag.
func (f *clientFactory) ResolveClusterArgument(ctx context.Context, nameOrID string) (string, error) {
	return f.resolveCluster(ctx, nameOrID, "")
}

// resolveCluster is ResolveCluster with its warning naming flag, "" for a
// positional value or a list entry.
func (f *clientFactory) resolveCluster(ctx context.Context, nameOrID, flag string) (string, error) {
	if nameOrID == "" || nameOrID == engine.LocalCluster {
		return engine.LocalCluster, nil
	}
	f.mu.Lock()
	if id, ok := f.resolved[nameOrID]; ok {
		f.mu.Unlock()
		return id, nil
	}
	f.mu.Unlock()
	get, list, err := f.clusters(ctx)
	if err != nil {
		return "", err
	}
	t := steve.ClusterText(nameOrID)
	t.Flag = flag
	id, warning, err := steve.ResolveCluster(ctx, get, list, nameOrID, t)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	if _, ok := f.resolved[nameOrID]; !ok {
		f.resolved[nameOrID] = id
		if warning != "" {
			f.notices = append(f.notices, warning)
		}
	}
	f.mu.Unlock()
	return id, nil
}

// DrainNotices implements runtime.NoticeSource: the warnings of the
// cluster lookups so far, each once.
func (f *clientFactory) DrainNotices() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.notices
	f.notices = nil
	return out
}

// noticeSources drains each source in order.
type noticeSources []runtime.NoticeSource

func (s noticeSources) DrainNotices() []string {
	var out []string
	for _, src := range s {
		out = append(out, src.DrainNotices()...)
	}
	return out
}

// runDeps bundles everything a command constructor might need for one run: the
// per-run cluster client factory, the file source for create's -f and patch's
// --patch-file input, and the resolved config dir/store. Each entry's
// constructor takes what it needs and ignores the rest, so a command is one
// entry row - no per-shape adapter or wiring switch.
type runDeps struct {
	clients    engine.ClusterClientFactory
	roots      engine.RootClientFactory
	serverBase engine.ServerBaseURLProvider
	src        engine.FileSource
	dir        string
	store      config.CredStore
	// anchor pairs the binding stamp read before this invocation's config
	// load with that load's Config, for the commands that read the
	// exec-credential cache.
	anchor       *config.CacheAnchor
	version      string
	sessionNamer engine.SessionTokenNamer
}

// adaptEntry wires one declarative entry through the single run path: it
// resolves the config dir/store, builds the client factory and file source,
// stamps server identity, derives the resolver binding, and runs the command
// through the pipeline. The registry probe passes a zero runDeps - a
// constructor MUST NOT touch its deps in Describe() (Describe is
// dir/store/client-independent); one that does would panic here rather than in
// production.
//
// When bare is true the action skips resolveDeps, newClientFactory, and
// newFileSource entirely and constructs the command with only version set.
// Schema, completion, and wireEntry validation are unaffected.
func adaptEntry(reg *runtime.Registry, version string, bare bool, make func(runDeps) runtime.Command) *ucli.Command {
	// probe is the SAME zero-runDeps instance used for both Describe() and the
	// ShellCompleter type-assert below - its dir field is permanently "". A
	// completer wired here is later called with this exact probe at real
	// TAB time too, so CompleteArgs must use only its dir PARAMETER, never
	// receiver state (a completer reading its own dir field would pass a unit
	// test built with a real dir and return empty forever at real TAB time).
	probe := make(runDeps{})
	d := probe.Describe()
	b := reg.Binding(d.Name)
	cmd := &ucli.Command{
		Name:        leafName(d.Name),
		Aliases:     d.Aliases,
		Usage:       d.Usage,
		Description: primeDescription(d),
		Metadata:    leafMetadata(d),
		ArgsUsage:   argsUsage(d.Args),
		Flags:       flagsFor(d),
		// Slice flags receive one entry per occurrence: urfave's default
		// comma-splitting would tear a value like `--set tags=a,b` into two
		// entries. Only slice/map flags read this; single-value
		// flags are untouched.
		DisableSliceFlagSeparator: true,
		OnUsageError:              onUsageError,
		Before: func(ctx context.Context, c *ucli.Command) (context.Context, error) {
			// Flags have already parsed successfully at this point, so the
			// invocation's own --output value is trusted directly.
			if ce := checkFlagValues(d.Flags, rawArgv, completionActive()); ce != nil {
				_ = runtime.RenderErrorStyled(c.Root().ErrWriter, c.String("output"), ce, errStyle(c, c.String("color")))
				return ctx, exit(ce)
			}
			return ctx, nil
		},
		Action: func(ctx context.Context, c *ucli.Command) error {
			io := buildIO(c)
			if ce := emptyConfigFlag(c); ce != nil {
				_ = runtime.RenderErrorStyled(c.Root().ErrWriter, io.Format, ce, errStyle(c, io.Color))
				return exit(ce)
			}
			var cmd runtime.Command
			if bare {
				cmd = make(runDeps{version: version})
			} else {
				// One resolution: the directory the command opens is the
				// one its remedies name, set before the config load. No load
				// error carries a remedy today; the order covers any later
				// one, and a test holds it.
				dir, err := config.ResolveDir(c.String("config"))
				if err != nil {
					return renderDepsError(c, io, err)
				}
				if d.RemedyConfigDir {
					io.RemedyDir = remedyDirFor(dir)
				}
				store, anchor, err := openDirectory(dir)
				if err != nil {
					return renderDepsError(c, io, err)
				}
				// The factory (a cluster lookup's warning) and the store (a
				// loose-permission finding, a keychain degrade) are the drain
				// handles for every notice recorded outside a Result, in that
				// order - wired here so renderErr/renderResult can drain them
				// ahead of rendering.
				f := newClientFactory(dir, store)
				sources := noticeSources{f}
				if ns, ok := store.(runtime.NoticeSource); ok {
					sources = append(sources, ns)
				}
				io.Notices = sources
				io.ServerIdentity = f.Identity
				io.Capability = newCapabilityChecker(f)
				io.Projects = projectResolver{f}
				cmd = make(runDeps{clients: f, roots: f, serverBase: f, src: newFileSource(io.In), dir: dir, store: store, anchor: anchor, version: version, sessionNamer: f})
			}
			status, ce := runtime.Run(ctx, cmd, cmdInput{c}, io, b)
			if ce != nil {
				return exit(ce)
			}
			if status != 0 {
				return ucli.Exit("", status)
			}
			return nil
		},
	}
	installShellComplete(probe, cmd)
	return cmd
}

// emptyConfigFlag refuses a blank --config. The flag also reads
// RANCHER_CLI_CONFIG_DIR, and urfave reports a value from the environment
// as set: a variable set to "" is the shell's way to unset it, so a blank
// value with the variable set to "" is not refused (a typed empty --config
// then cannot be told apart and runs as the default directory); a value
// equal to a blank variable is refused naming the variable.
func emptyConfigFlag(c *ucli.Command) *clierr.Error {
	v := c.String("config")
	if !c.IsSet("config") || strings.TrimSpace(v) != "" {
		return nil
	}
	env, set := os.LookupEnv(config.EnvConfigDir)
	switch {
	case set && env == "":
		return nil
	case set && env == v:
		return clierr.New(clierr.CodeValidation,
			config.EnvConfigDir+" is blank; set it to a directory or unset it")
	default:
		return runtime.EmptyValueError("config")
	}
}
