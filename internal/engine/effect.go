package engine

import (
	"context"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// PlanCreate previews a create via server-side dry-run and returns the Effect
// plus the closure that performs the real create. Tier/Stability are stamped
// by the pipeline, not set here. resultKind names the Result envelope kind.
// effectCluster is the cluster the mutation semantically targets — NOT the
// proxy the client connects through. A Project is written via the management
// proxy ("local") but lands in a downstream cluster; the Effect must show the
// downstream cluster (PE-5), so routing and identity are separate parameters.
func PlanCreate(ctx context.Context, client ClusterClient, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, effectCluster, resultKind string) (safety.Effect, runtime.ExecuteFunc, error) {
	out, err := client.Create(ctx, gvr, ns, obj, true)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	eff := safety.Effect{
		Action:            safety.ActionCreate,
		Target:            gvr.Resource + "/" + out.GetName(),
		Cluster:           effectCluster,
		ConflictDetection: true,
		Reversible:        true,
		Destructive:       false,
	}
	exec := func(ctx context.Context) (runtime.Result, error) {
		created, err := client.Create(ctx, gvr, ns, obj, false)
		if err != nil {
			return runtime.Result{}, err
		}
		return runtime.Result{Kind: resultKind, Data: created.Object}, nil
	}
	return eff, exec, nil
}
