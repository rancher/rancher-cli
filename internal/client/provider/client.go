package provider

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rancher/rancher-cli/internal/client/kube"
	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// requestTimeout bounds every non-streaming API operation. Watch/stream
// calls (none yet) must exempt themselves at the call site.
const requestTimeout = 30 * time.Second

func opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, requestTimeout)
}

type dynamicClient struct {
	dyn  dynamic.Interface
	disc discovery.DiscoveryInterface
	cs   kubernetes.Interface

	mu       sync.Mutex
	versions map[string]string // group → resolved preferred version
}

// NewClusterClient builds the dynamic-client ClusterClient for a rest.Config
// (one cluster proxy). The discovery client is built from the bounded config
// copy (discoveryConfig) so an empty-version resolution cannot re-open the
// unbounded-call hang class Preflight closed: ServerGroups() takes no context.
func NewClusterClient(rc *rest.Config) (engine.ClusterClient, error) {
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
	return &dynamicClient{dyn: dyn, disc: disc, cs: cs, versions: map[string]string{}}, nil
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
func (r *dynamicClient) resolveVersion(gvr schema.GroupVersionResource) (schema.GroupVersionResource, error) {
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
	// Discovery runs OUTSIDE the lock: ServerGroups() is a network round-trip
	// bounded by the discovery config's transport timeout, and holding r.mu
	// across it would serialize every concurrent version-resolving call behind
	// one slow endpoint. Benign race by design — concurrent missers each
	// resolve and converge on the same value (mirrors the factory's
	// ResolveCluster memo). Do NOT reintroduce a lock around the call below.
	groups, err := r.disc.ServerGroups()
	if err != nil {
		return gvr, fmt.Errorf("discovering version for group %q: %w", gvr.Group, err)
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
				fmt.Sprintf("API group %q has no preferred version", gvr.Group))
		}
		r.mu.Lock()
		r.versions[gvr.Group] = g.PreferredVersion.Version
		r.mu.Unlock()
		gvr.Version = g.PreferredVersion.Version
		return gvr, nil
	}
	if gvr.Group == "ext.cattle.io" {
		return gvr, clierr.New(clierr.CodeNotFound,
			"this Rancher does not serve ext.cattle.io (requires Rancher >= 2.12)").
			WithHint("token management: auth list-tokens; kubeconfig: the default exec-credential mode")
	}
	return gvr, clierr.New(clierr.CodeNotFound, fmt.Sprintf("API group %q not served by the cluster", gvr.Group))
}

func (r *dynamicClient) Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	gvr, err := r.resolveVersion(gvr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	out, err := r.ri(gvr, ns).Get(ctx, name, metav1.GetOptions{})
	return out, mapErr(err)
}

func (r *dynamicClient) List(ctx context.Context, gvr schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error) {
	gvr, err := r.resolveVersion(gvr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	out, err := r.ri(gvr, ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	return out, mapErr(err)
}

// Logs fetches the last tailLines lines of a container's log via the typed
// clientset (pod logs have no dynamic/unstructured API).
func (r *dynamicClient) Logs(ctx context.Context, ns, pod, container string, tailLines int) (string, error) {
	ctx, cancel := opCtx(ctx)
	defer cancel()
	tl := int64(tailLines)
	req := r.cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, TailLines: &tl})
	b, err := req.DoRaw(ctx)
	if err != nil {
		return "", mapErr(err)
	}
	return string(b), nil
}

func (r *dynamicClient) Create(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	gvr, err := r.resolveVersion(gvr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	opts := metav1.CreateOptions{}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	out, err := r.ri(gvr, ns).Create(ctx, obj, opts)
	return out, mapErr(err)
}

func (r *dynamicClient) Update(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	gvr, err := r.resolveVersion(gvr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := opCtx(ctx)
	defer cancel()
	opts := metav1.UpdateOptions{}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	out, err := r.ri(gvr, ns).Update(ctx, obj, opts)
	return out, mapErr(err)
}

func (r *dynamicClient) Delete(ctx context.Context, gvr schema.GroupVersionResource, ns, name, resourceVersion string, dryRun bool) error {
	gvr, err := r.resolveVersion(gvr)
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
	return mapErr(r.ri(gvr, ns).Delete(ctx, name, opts))
}
