package engine

import (
	"context"

	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// PlanCreateObserved previews a create via server-side dry-run and returns
// the Effect, the SERVER's dry-run response, and the closure that performs
// the real create. The dry-run response is exposed so a caller can compare
// its submission against what the server accepted (create machine-config's
// pruned-key warning, MC-12); PlanCreate delegates here, so every existing
// create keeps its Effect byte-for-byte. Tier/Stability are stamped by the
// pipeline, not set here. resultKind names the Result envelope kind.
// effectCluster is the cluster the mutation semantically targets — NOT the
// proxy the client connects through (see PlanCreate's original PE-5 note).
// destructive is the caller's per-kind policy.
func PlanCreateObserved(ctx context.Context, client ClusterClient, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, effectCluster, resultKind string, destructive bool) (safety.Effect, *unstructured.Unstructured, runtime.ExecuteFunc, error) {
	out, err := client.Create(ctx, gvr, ns, obj, true)
	if err != nil {
		return safety.Effect{}, nil, nil, err
	}
	eff := safety.Effect{
		Action:            safety.ActionCreate,
		Target:            gvr.Resource + "/" + out.GetName(),
		Cluster:           effectCluster,
		ConflictDetection: true,
		Reversible:        true,
		Destructive:       destructive,
	}
	exec := func(ctx context.Context) (runtime.Result, error) {
		created, err := client.Create(ctx, gvr, ns, obj, false)
		if err != nil {
			return runtime.Result{}, err
		}
		return runtime.Result{Kind: resultKind, Data: created.Object}, nil
	}
	return eff, out, exec, nil
}

// PlanCreate previews a create via server-side dry-run and returns the
// Effect plus the closure that performs the real create — the observed
// variant with the dry-run response discarded. See PlanCreateObserved for
// the parameter contract.
func PlanCreate(ctx context.Context, client ClusterClient, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, effectCluster, resultKind string, destructive bool) (safety.Effect, runtime.ExecuteFunc, error) {
	eff, _, exec, err := PlanCreateObserved(ctx, client, gvr, ns, obj, effectCluster, resultKind, destructive)
	return eff, exec, err
}

// PlanUpdate previews an update via server-side dry-run and returns the Effect
// plus the closure that performs the real update. The mutation is fully decided
// by the caller: obj is the already-transformed object carrying the
// resourceVersion precondition, diff is the precomputed field-level change, and
// destructive is the caller's per-kind policy. Tier/Stability are stamped by
// the pipeline. Mirrors PlanCreate so create and update share one shape.
func PlanUpdate(ctx context.Context, client ClusterClient, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, effectCluster, resultKind string, diff []safety.DiffEntry, destructive bool) (safety.Effect, runtime.ExecuteFunc, error) {
	if _, err := client.Update(ctx, gvr, ns, obj, true); err != nil {
		return safety.Effect{}, nil, err
	}
	eff := safety.Effect{
		Action:            safety.ActionUpdate,
		Target:            gvr.Resource + "/" + obj.GetName(),
		Cluster:           effectCluster,
		Diff:              diff,
		ResourceVersion:   obj.GetResourceVersion(),
		ConflictDetection: true,
		Reversible:        true,
		Destructive:       destructive,
	}
	exec := func(ctx context.Context) (runtime.Result, error) {
		updated, err := client.Update(ctx, gvr, ns, obj, false)
		if err != nil {
			return runtime.Result{}, err
		}
		return runtime.Result{Kind: resultKind, Data: updated.Object}, nil
	}
	return eff, exec, nil
}
