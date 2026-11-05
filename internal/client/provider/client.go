package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// requestTimeout bounds every non-streaming API operation. WatchObject is the
// one exemption: its stream lasts as long as the server keeps it open, and
// only its open, until the response headers, is bounded by this value. A package var
// (rather than a constant) so tests can shrink it via
// SetRequestTimeoutForTest - the keyringProbeTimeout test-shrinkable
// pattern in internal/config/credstore.go.
var requestTimeout = 30 * time.Second

// SetRequestTimeoutForTest shrinks the per-request timeout and returns a
// restore func. In non-test code so other packages' tests can reach it -
// mirrors config.SetExecHelperTimeoutForTest. Callers must not run in
// parallel with tests issuing real requests.
func SetRequestTimeoutForTest(d time.Duration) (restore func()) {
	old := requestTimeout
	requestTimeout = d
	return func() { requestTimeout = old }
}

// opCtx bounds one operation and makes sure its context records what the
// server answers (kube.WithAnswers); a context that records already keeps
// its record, so a call inside another shares it.
func opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(kube.WithAnswers(ctx), requestTimeout)
}

// bootstrapLocalDynamic builds a dynamic client bound to the LOCAL cluster's
// proxy plus a request-bounded context, for the small `auth status` probes
// (Identity, EffectiveTokenMetadata, V3TokenMetadata) that each read a
// single management-plane singleton. ext.cattle.io and
// management.cattle.io resources live under the local cluster's proxy,
// which is exactly where kube.RESTConfig aims. The returned cancel func
// must be deferred by the caller exactly like the opCtx it wraps.
func bootstrapLocalDynamic(ctx context.Context, eff config.Effective) (dynamic.Interface, context.Context, context.CancelFunc, error) {
	rc, err := kube.RESTConfig(eff, engine.LocalCluster)
	if err != nil {
		return nil, nil, nil, err
	}
	dyn, err := kube.Dynamic(rc)
	if err != nil {
		return nil, nil, nil, err
	}
	boundedCtx, cancel := opCtx(ctx)
	return dyn, boundedCtx, cancel, nil
}

type dynamicClient struct {
	dyn dynamic.Interface
	cs  kubernetes.Interface

	// groups reads the server's API group list with the caller's context on
	// the request; resolveVersion consults it for an empty GVR version.
	groups func(ctx context.Context) (*metav1.APIGroupList, error)

	// hc and host serve the raw review POSTs: an authorization review has no
	// dynamic-client shape that keeps the answer's HTTP status and raw body.
	hc   *http.Client // authenticated transport
	host string       // rest.Config.Host: https://<rancher>/k8s/clusters/<id>

	// tbl serves the table reads (ListTable, GetTable): the same
	// connection settings as dyn, with a decoder for the server's Table.
	tbl rest.Interface

	// clusterID is the --cluster value this client's proxy targets, carried
	// only so a curated NOT_FOUND hint for a Downstream-scoped kind can name
	// a complete list command - it plays no role in request routing itself.
	clusterID string

	mu       sync.Mutex
	versions map[string]string // group → resolved preferred version
}

// NewClusterClient builds the dynamic-client ClusterClient for a rest.Config
// (one cluster proxy) and the --cluster value the caller resolved that proxy
// from. The discovery client is built from the bounded config copy
// (discoveryConfig) so an empty-version resolution has the same request
// timeout as every other call, on top of the caller's context.
//
// Without WithWatch the client is the *dynamicClient, which has no ListObject
// or WatchObject method; with it, a *watchClient that adds them.
func NewClusterClient(rc *rest.Config, clusterID string, opts ...Option) (engine.ClusterClient, error) {
	var o clientOptions
	for _, opt := range opts {
		opt(&o)
	}
	dyn, err := kube.Dynamic(rc)
	if err != nil {
		return nil, err
	}
	disc, err := kube.Discovery(discoveryConfig(rc))
	if err != nil {
		return nil, err
	}
	cs, err := kube.Clientset(rc)
	if err != nil {
		return nil, err
	}
	hc, err := kube.HTTPClient(rc)
	if err != nil {
		return nil, err
	}
	tbl, err := kube.TableClient(rc)
	if err != nil {
		return nil, err
	}
	dc := &dynamicClient{
		dyn: dyn, cs: cs, hc: hc, tbl: tbl, host: strings.TrimRight(rc.Host, "/"),
		groups: func(ctx context.Context) (*metav1.APIGroupList, error) {
			return serverGroups(ctx, disc)
		},
		clusterID: clusterID, versions: map[string]string{},
	}
	if o.watch {
		return &watchClient{dynamicClient: dc}, nil
	}
	return dc, nil
}

func (r *dynamicClient) ri(gvr schema.GroupVersionResource, ns string) dynamic.ResourceInterface {
	res := r.dyn.Resource(gvr)
	if ns == "" {
		return res
	}
	return res.Namespace(ns)
}

// resolveVersion fills an empty GVR version from discovery (the group's
// preferred version), memoized per group. MCP leaves cluster.x-k8s.io versions
// empty for the same reason: the served version varies by Rancher release, so
// pinning one breaks across installs.
func (r *dynamicClient) resolveVersion(ctx context.Context, gvr schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	if gvr.Version != "" {
		return gvr, nil
	}
	r.mu.Lock()
	v, ok := r.versions[gvr.Group]
	r.mu.Unlock()
	if ok {
		gvr.Version = v
		return gvr, nil
	}
	// Discovery runs OUTSIDE the lock: the group list is a network round-trip
	// bounded by ctx and the discovery config's request timeout, and holding r.mu
	// across it would serialize every concurrent version-resolving call behind
	// one slow endpoint. Benign race by design - concurrent missers each
	// resolve and converge on the same value (mirrors the factory's
	// ResolveCluster memo). Do NOT reintroduce a lock around the call below.
	groups, err := r.groups(ctx)
	if err != nil {
		return gvr, clierr.WrapUnlessCanceled(err, func(err error) error {
			return mapRequestErr(ctx, http.MethodGet, fmt.Errorf("discovering version for group %q: %w", gvr.Group, err),
				&clierr.Request{Cluster: r.clusterID, Endpoint: true, Server: r.server()})
		})
	}
	for _, g := range groups.Groups {
		if g.Name != gvr.Group {
			continue
		}
		// An empty preferred version must NOT be cached: memoizing "" would
		// poison the group so every future call returns "" and fails with a
		// malformed URL, with no recovery short of recreating the client. Leave
		// it unset so a later call retries once the group reports a version.
		if g.PreferredVersion.Version == "" {
			return gvr, clierr.New(clierr.CodeNotFound,
				fmt.Sprintf("API group %q has no preferred version; the server may be mid-upgrade; retry, or check API requirements with: rancher schema <kind>", gvr.Group))
		}
		r.mu.Lock()
		r.versions[gvr.Group] = g.PreferredVersion.Version
		r.mu.Unlock()
		gvr.Version = g.PreferredVersion.Version
		return gvr, nil
	}
	if gvr.Group == "ext.cattle.io" {
		return gvr, clierr.New(clierr.CodeNotFound,
			fmt.Sprintf("this Rancher does not serve ext.cattle.io (requires Rancher >= %s, the supported minimum); "+
				"token management: %s; kubeconfig: the default exec-credential mode", config.MinRancherVersion, clierr.Rancher+" auth list-tokens"))
	}
	return gvr, clierr.New(clierr.CodeNotFound,
		fmt.Sprintf("API group %q not served by the cluster; the kind may need a newer server version or an optional feature; check the requirements with: rancher schema <kind>", gvr.Group))
}

// curateNotFound enriches a hint-less NOT_FOUND error with the curated
// "<kind> not found: <name>" message and a list hint when the GVR resolves to a
// known kind row. The has-hint guard protects any hinted NOT_FOUND arriving at
// this seam - resolveVersion diagnostics return early from the caller before
// reaching curateNotFound, so they are not protected by this guard but by that
// early return. Errors from GVRs not in the kinds table pass through unchanged.
// LIST and Create callers must not call this; only the named-object methods
// (Get, Delete, Update) do.
func (r *dynamicClient) curateNotFound(gvr schema.GroupVersionResource, name string, err error) error {
	if ce := clierr.AsNotFound(err); ce != nil && ce.Hint == "" {
		if k, ok := kinds.ByGVR(gvr); ok {
			curated := kinds.NotFoundCurated(k, name, r.clusterID)
			if cc, ok := errors.AsType[*clierr.Error](curated); ok {
				return cc.WithCause(err)
			}
			return curated
		}
	}
	return err
}

// server is the server this client talks to, as output may show it.
func (r *dynamicClient) server() string { return config.DisplayServerURL(r.host) }

// request describes one call of this client for the error mapping.
func (r *dynamicClient) request(verb string, gvr schema.GroupVersionResource, ns, name string) *clierr.Request {
	return &clierr.Request{Verb: verb, Group: gvr.Group, Resource: gvr.Resource, Name: name, Cluster: r.clusterID, Server: r.server()}
}

// forbiddenHint adds the caller-scoped command kinds.ForbiddenHint names
// for an RBAC refusal of a kind verb that carries no hint yet.
func (r *dynamicClient) forbiddenHint(gvr schema.GroupVersionResource, verb, ns, name string, err error) error {
	ce, ok := errors.AsType[*clierr.Error](err)
	if !ok || ce.Code != clierr.CodeRBAC || ce.Hint != "" {
		return err
	}
	if k, ok := kinds.ByGVR(gvr); ok {
		if h := kinds.ForbiddenHint(k, verb, ns, name); h != "" {
			ce.Hint = h
		}
	}
	return err
}

func (r *dynamicClient) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	ctx = kube.WithAnswers(ctx)
	gvr, err := r.resolveVersion(ctx, gvr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	out, err := r.ri(gvr, ns).Get(ctx, name, metav1.GetOptions{})
	return out, r.forbiddenHint(gvr, "get", ns, name, r.curateNotFound(gvr, name, mapRequestErr(ctx, http.MethodGet, err, r.request("get", gvr, ns, name))))
}

func (r *dynamicClient) List(ctx context.Context, gvr schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error) {
	ctx = kube.WithAnswers(ctx)
	gvr, err := r.resolveVersion(ctx, gvr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	out, err := r.ri(gvr, ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	return out, r.forbiddenHint(gvr, "list", ns, "", mapRequestErr(ctx, http.MethodGet, err, r.request("list", gvr, ns, "")))
}

// Logs fetches a container's log via the typed clientset (pod logs have no
// dynamic/unstructured API).
func (r *dynamicClient) Logs(ctx context.Context, ns, pod string, opts engine.LogOptions) (string, error) {
	ctx, cancel := opCtx(ctx)
	defer cancel()
	req := r.cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container:    opts.Container,
		TailLines:    opts.TailLines,
		SinceSeconds: opts.SinceSeconds,
		Previous:     opts.Previous,
		Timestamps:   opts.Timestamps,
	})
	b, err := req.DoRaw(ctx)
	if err != nil {
		return "", mapRequestErr(ctx, http.MethodGet, restoreStatus(err, b),
			&clierr.Request{Verb: "get", Resource: "pods/log", Name: pod, Cluster: r.clusterID, Server: r.server()})
	}
	return string(b), nil
}

func (r *dynamicClient) Create(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	ctx = kube.WithAnswers(ctx)
	gvr, err := r.resolveVersion(ctx, gvr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	opts := metav1.CreateOptions{FieldManager: kube.FieldManager}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	out, err := r.ri(gvr, ns).Create(ctx, obj, opts)
	return out, mapRequestErr(ctx, http.MethodPost, err, r.request("create", gvr, ns, obj.GetName()))
}

func (r *dynamicClient) Update(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	ctx = kube.WithAnswers(ctx)
	gvr, err := r.resolveVersion(ctx, gvr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	opts := metav1.UpdateOptions{FieldManager: kube.FieldManager}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	out, err := r.ri(gvr, ns).Update(ctx, obj, opts)
	return out, r.forbiddenHint(gvr, "update", ns, obj.GetName(),
		r.curateNotFound(gvr, obj.GetName(), mapRequestErr(ctx, http.MethodPut, err, r.request("update", gvr, ns, obj.GetName()))))
}

func (r *dynamicClient) Delete(ctx context.Context, gvr schema.GroupVersionResource, ns, name, resourceVersion string, dryRun bool) error {
	ctx = kube.WithAnswers(ctx)
	gvr, err := r.resolveVersion(ctx, gvr)
	if err != nil {
		return err
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	opts := metav1.DeleteOptions{}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	if resourceVersion != "" {
		opts.Preconditions = &metav1.Preconditions{ResourceVersion: &resourceVersion}
	}
	return r.curateNotFound(gvr, name, mapRequestErr(ctx, http.MethodDelete, r.ri(gvr, ns).Delete(ctx, name, opts), r.request("delete", gvr, ns, name)))
}
