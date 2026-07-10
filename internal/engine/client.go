// Package engine holds pure domain operations. It depends on the runtime
// contracts and safety types only — never on the provider adapter, urfave,
// or the terminal.
package engine

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// LocalCluster is the management cluster's proxy id.
const LocalCluster = "local"

// FileSource is the port mutation commands use to read manifest/patch input.
// Path "-" reads the invocation's stdin. The surface implements it; engine
// code never touches os.Stdin (surface-neutral engine).
type FileSource interface {
	Read(path string) ([]byte, error)
}

// ClusterClient is the port for one cluster's Rancher-proxied k8s API
// (https://<rancher>/k8s/clusters/<id>). The dynamic implementation lives in
// internal/client/provider. Cluster-scoped resources pass ns == "". Writes
// take a dryRun flag (server-side dry-run). Delete takes an optional
// resourceVersion precondition ("" = none).
type ClusterClient interface {
	Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error)
	// List lists objects in ns; selector "" lists all (no label filter).
	List(ctx context.Context, gvr schema.GroupVersionResource, ns, selector string) (*unstructured.UnstructuredList, error)
	Create(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error)
	Update(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error)
	Delete(ctx context.Context, gvr schema.GroupVersionResource, ns, name, resourceVersion string, dryRun bool) error
	// Logs fetches a container's log per opts.
	Logs(ctx context.Context, ns, pod string, opts LogOptions) (string, error)
}

// LogOptions parameterizes a Logs call. SinceSeconds and TailLines are both
// pointers because nil (unset) and *0 (an explicit zero) are different
// requests to corev1.PodLogOptions — SinceSeconds' nil means "no limit" vs
// *0 meaning "since now" (silently empty output); TailLines' nil means "no
// limit" vs *0 meaning "request exactly zero lines". A plain int64 here
// would let a caller that forgets to set TailLines silently ask for zero
// lines instead of "no limit". Callers that only have a duration/count
// string must decide which one they mean before constructing this struct.
type LogOptions struct {
	Container    string
	TailLines    *int64
	SinceSeconds *int64
	Previous     bool
	Timestamps   bool
}

// ClusterClientFactory hands out per-cluster clients at call time (network
// deferred; memoized per invocation).
//
// Server identity for Effect stamping is NOT part of this port: only the
// surface adapter needs it, and it reads it off the concrete factory
// (io.ServerIdentity) rather than through the engine-facing interface (ISP).
type ClusterClientFactory interface {
	Cluster(ctx context.Context, clusterID string) (ClusterClient, error)
	// ContextCluster returns the cluster from the resolved current context
	// ("" if the context sets none). Commands that target "whatever the
	// context says" must call this explicitly — implicit context targeting
	// via an empty cluster id is forbidden (RF-2).
	ContextCluster() (string, error)
	// ContextProject returns the project from the resolved current context
	// ("" if the context sets none). Routing consults it only as a fallback
	// for an empty --project flag (PC-2 precedence: flag > context).
	ContextProject() (string, error)
	// ResolveCluster maps a cluster display name or id to its cluster id
	// (rancher-ai-mcp GetClusterID semantics); "" and LocalCluster
	// short-circuit to LocalCluster. Memoized per invocation.
	ResolveCluster(ctx context.Context, nameOrID string) (string, error)
}
