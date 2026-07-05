// Package engine holds pure domain operations. It depends on the Repo port and
// safety types only — never on the provider adapter, urfave, or stdout.
package engine

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Repo is the port the engine uses to read/write Rancher resources. The dynamic
// implementation lives in internal/client/provider. Cluster-scoped resources pass
// ns == "". Writes take a dryRun flag (server-side dry-run). Delete takes an
// optional resourceVersion precondition ("" = none).
type Repo interface {
	Get(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error)
	List(ctx context.Context, gvr schema.GroupVersionResource, ns string) (*unstructured.UnstructuredList, error)
	Create(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error)
	Update(ctx context.Context, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error)
	Delete(ctx context.Context, gvr schema.GroupVersionResource, ns, name, resourceVersion string, dryRun bool) error
}

// RepoFactory builds a Repo bound to a specific cluster proxy (e.g. "local" for
// the management cluster, or a downstream cluster id). Network is deferred to call
// time.
type RepoFactory func(clusterID string) (Repo, error)
