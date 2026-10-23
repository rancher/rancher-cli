package engine

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// PlanCreateObserved previews a create via server-side dry-run and returns
// the Effect, the SERVER's dry-run response, and the closure that performs
// the real create. The dry-run response is exposed so a caller can compare
// its submission against what the server accepted (create machine-config's
// pruned-key warning); PlanCreate delegates here. The Effect's diff lists
// the fields the command sends (CreateDiff), computed from obj and not from
// the server's answer, with the values redaction hides; the server's dry-run
// still decides whether the preview succeeds. redaction is the redaction of
// the resource gvr names (kinds.Kind.Redaction of its row, or
// kinds.RowlessRedaction): the zero value, or the redaction of another
// resource, is an internal error refused before any request. The Effect
// carries no contract/stability - the resolver Binding drives the
// pipeline's gates and the manifest is the only published source. resultKind names the Result
// envelope kind.
// effectCluster is the cluster the mutation semantically targets - NOT the
// proxy the client connects through.
// displayKind is the kind table's canonical CLI name - the ONLY source for
// default-format text (never resultKind, which is the envelope Result.Kind
// and may be a Kubernetes/API identity such as "Cluster" or "Secret").
// destructive is the caller's per-kind policy.
func PlanCreateObserved(ctx context.Context, client ClusterClient, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, redaction kinds.Redaction, effectCluster, displayKind, resultKind string, destructive bool) (safety.Effect, *unstructured.Unstructured, runtime.ExecuteFunc, error) {
	if err := checkRedaction(gvr, redaction); err != nil {
		return safety.Effect{}, nil, nil, err
	}
	diff := createDiff(obj.Object, redaction)
	out, err := client.Create(ctx, gvr, ns, obj, true)
	if err != nil {
		return safety.Effect{}, nil, nil, err
	}
	eff := safety.Effect{
		Action:            safety.ActionCreate,
		Target:            gvr.Resource + "/" + out.GetName(),
		DisplayKind:       displayKind,
		DisplayName:       out.GetName(),
		Cluster:           effectCluster,
		Diff:              diff,
		ConflictDetection: true,
		Reversible:        true,
		Destructive:       destructive,
	}
	exec := func(ctx context.Context) (runtime.Result, error) {
		created, err := client.Create(ctx, gvr, ns, obj, false)
		if err != nil {
			return runtime.Result{}, err
		}
		// The committed identity is the RETURNED object's name - the real,
		// server-assigned name (including a generateName result) - never
		// the dry-run preview's Effect.DisplayName, which a generateName
		// caller (grant) may have rewritten to a "prefix*" placeholder.
		res := runtime.MutationText(displayKind, created.GetName(), "created")
		res.Kind = resultKind
		res.Data = created.Object
		return res, nil
	}
	return eff, out, exec, nil
}

// PlanCreate previews a create via server-side dry-run and returns the
// Effect plus the closure that performs the real create - the observed
// variant with the dry-run response discarded. See PlanCreateObserved for
// the parameter contract.
func PlanCreate(ctx context.Context, client ClusterClient, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, redaction kinds.Redaction, effectCluster, displayKind, resultKind string, destructive bool) (safety.Effect, runtime.ExecuteFunc, error) {
	eff, _, exec, err := PlanCreateObserved(ctx, client, gvr, ns, obj, redaction, effectCluster, displayKind, resultKind, destructive)
	return eff, exec, err
}

// PlanUpdate previews an update via server-side dry-run and returns the Effect
// plus the closure that performs the real update. The mutation is fully decided
// by the caller: obj is the already-transformed object carrying the
// resourceVersion precondition, diff is the precomputed field-level change, and
// destructive is the caller's per-kind policy. displayKind is the kind table's
// canonical CLI name (see PlanCreateObserved). The Effect carries no
// contract/stability (see PlanCreateObserved). Mirrors PlanCreate so create
// and update share one shape.
func PlanUpdate(ctx context.Context, client ClusterClient, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, effectCluster, displayKind, resultKind string, diff []safety.DiffEntry, destructive bool) (safety.Effect, runtime.ExecuteFunc, error) {
	eff, _, exec, err := PlanUpdateObserved(ctx, client, gvr, ns, obj, effectCluster, displayKind, resultKind, diff, destructive)
	return eff, exec, err
}

// PlanUpdateObserved is PlanUpdate that also returns the server's dry-run
// answer: the object as the update would store it, with the server's
// defaults and admission changes applied.
func PlanUpdateObserved(ctx context.Context, client ClusterClient, gvr schema.GroupVersionResource, ns string, obj *unstructured.Unstructured, effectCluster, displayKind, resultKind string, diff []safety.DiffEntry, destructive bool) (safety.Effect, *unstructured.Unstructured, runtime.ExecuteFunc, error) {
	out, err := client.Update(ctx, gvr, ns, obj, true)
	if err != nil {
		return safety.Effect{}, nil, nil, err
	}
	eff := safety.Effect{
		Action:            safety.ActionUpdate,
		Target:            gvr.Resource + "/" + obj.GetName(),
		DisplayKind:       displayKind,
		DisplayName:       obj.GetName(),
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
		res := runtime.MutationText(displayKind, updated.GetName(), "patched")
		res.Kind = resultKind
		res.Data = updated.Object
		return res, nil
	}
	return eff, out, exec, nil
}
