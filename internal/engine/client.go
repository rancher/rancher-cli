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

// ClusterClient is the port for one cluster's Rancher-proxied k8s API
// (https://<rancher>/k8s/clusters/<id>). The dynamic implementation lives in
// internal/client/provider. Cluster-scoped resources pass ns == "". Writes
// take a dryRun flag (server-side dry-run). Delete takes an optional
// resourceVersion precondition ("" = none).
type ClusterClient interface {
	Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error)
	List(ctx context.Context, gvr schema.GroupVersionResource, ns string) (*unstructured.UnstructuredList, error)
	Create(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error)
	Update(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error)
	Delete(ctx context.Context, gvr schema.GroupVersionResource, ns, name, resourceVersion string, dryRun bool) error
}

// ClusterClientFactory hands out per-cluster clients at call time (network
// deferred; memoized per invocation).
//
// TODO: add ResolveCluster (display-name → id) when generic verbs land.
type ClusterClientFactory interface {
	Cluster(ctx context.Context, clusterID string) (ClusterClient, error)
	// ContextCluster returns the cluster from the resolved current context
	// ("" if the context sets none). Commands that target "whatever the
	// context says" must call this explicitly — implicit context targeting
	// via an empty cluster id is forbidden (RF-2).
	ContextCluster() (string, error)
	// Identity returns the resolved server name for Effect stamping (PE-5).
	Identity() (server string, err error)
}
