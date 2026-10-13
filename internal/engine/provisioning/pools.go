// pools.go implements the cluster-family verbs' shared machine-pool
// resolution: fetching a provisioning Cluster by name and locating a pool by
// name - never by array index, so no user-facing surface ever exposes a
// positional index that could shift between read and patch (the
// resourceVersion precondition, not a stale index, is what catches a pool
// added or removed concurrently) - plus the fetch→resolve→mutate→diff→plan
// spine every pool-mutating verb (scale, set-autoscaling, clear-autoscaling)
// shares.
package provisioning

import (
	"context"
	"fmt"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/runtime"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// clusterAutoscalerPausedAnnotation is the operator-facing pause switch on
// the provisioning Cluster (capr/common.go: the template controller
// propagates it to the CAPI Cluster, where the autoscaler controller reads
// it). Pause is cluster-wide only - no per-pool pause exists server-side.
const clusterAutoscalerPausedAnnotation = "provisioning.cattle.io/cluster-autoscaler-paused"

// clusterNamespaceFlag is the shared --namespace flag every cluster-family
// verb exposes, defaulting to clusterNamespace (fleet-default) when absent -
// the same DefaultNamespace `delete provisioning-cluster` resolves against.
// Unlike `create cluster`, which fixes the namespace with no flag, a family
// verb's target may live in another fleet workspace.
func clusterNamespaceFlag() runtime.ArgSpec {
	f := engine.NamespaceFlag(false)
	f.Desc = "fleet workspace namespace of the provisioning cluster (default: fleet-default)"
	return f
}

// resolveClusterNamespace reads --namespace, falling back to fleet-default.
func resolveClusterNamespace(in runtime.Input) string {
	if ns := in.String("namespace"); ns != "" {
		return ns
	}
	return clusterNamespace
}

// fetchClusterFamilyTarget validates the required <cluster> positional and
// GETs the provisioning Cluster object every family verb mutates, on the
// local management cluster (the provisioning Cluster is a management-plane
// resource, not a downstream one).
func fetchClusterFamilyTarget(ctx context.Context, clients engine.ClusterClientFactory, in runtime.Input) (client engine.ClusterClient, obj *unstructured.Unstructured, ns, name string, err error) {
	args := in.Args()
	if len(args) == 0 || args[0] == "" {
		return nil, nil, "", "", clierr.New(clierr.CodeValidation, "argument <cluster> is required")
	}
	name = args[0]
	ns = resolveClusterNamespace(in)
	client, err = clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return nil, nil, "", "", err
	}
	obj, err = client.Get(ctx, provisioningClusterGVR, ns, name)
	if err != nil {
		return nil, nil, "", "", err
	}
	return client, obj, ns, name, nil
}

// clusterMachinePools reads spec.rkeConfig.machinePools as a fresh, mutable
// copy (unstructured.NestedSlice always deep-copies) ready for in-place pool
// mutation and a matching SetNestedSlice write-back. found reports only
// whether the field itself is present - a present-but-empty list (found,
// zero length) is a different shape from a wholly absent one, and callers
// that treat both as "no pools" must say so accurately.
func clusterMachinePools(cluster *unstructured.Unstructured) (pools []any, found bool) {
	pools, found, _ = unstructured.NestedSlice(cluster.Object, "spec", "rkeConfig", "machinePools")
	return pools, found
}

// findMachinePool locates poolName's index in pools by exact name match -
// the CLI's own name→index mapping, computed fresh from the read that also
// backs the resourceVersion precondition.
func findMachinePool(pools []any, poolName string) (index int, ok bool) {
	for i, p := range pools {
		pm, isMap := p.(map[string]any)
		if !isMap {
			continue
		}
		if n, _, _ := unstructured.NestedString(pm, "name"); n == poolName {
			return i, true
		}
	}
	return 0, false
}

// poolAutoscaled reports whether pool carries either autoscaling bound. The
// CRD's write-side CEL requires both or neither, but a half-set pool (should
// that CEL ever be bypassed) is still treated as autoscaled defensively.
func poolAutoscaled(pool map[string]any) bool {
	_, minFound, _ := unstructured.NestedInt64(pool, "autoscalingMinSize")
	_, maxFound, _ := unstructured.NestedInt64(pool, "autoscalingMaxSize")
	return minFound || maxFound
}

// poolRequiresPositiveMin reports whether pool carries a control-plane or
// etcd role - the CRD's CEL requires autoscalingMinSize > 0 on such a pool
// (a control-plane/etcd pool can never scale to zero members).
func poolRequiresPositiveMin(pool map[string]any) bool {
	etcd, _, _ := unstructured.NestedBool(pool, "etcdRole")
	cp, _, _ := unstructured.NestedBool(pool, "controlPlaneRole")
	return etcd || cp
}

// autoscalingPaused reports whether cluster carries the pause annotation.
// The strict "true" comparison mirrors the server's own check
// (rancher capr: the autoscaler controller reads this annotation with the
// identical strict compare) - an operator-set "True" is not paused
// server-side either, so tolerance here would diverge from server truth.
func autoscalingPaused(cluster *unstructured.Unstructured) bool {
	v, _, _ := unstructured.NestedString(cluster.Object, "metadata", "annotations", clusterAutoscalerPausedAnnotation)
	return v == "true"
}

// poolLessClusterError refuses a family verb against a cluster with no
// machine pools to operate on - never the raw server 4xx or no-op an
// absent-pools shape would otherwise produce. fieldPresent distinguishes a
// wholly pool-less cluster (imported, custom, or local - the field itself
// is absent) from one whose machinePools field exists but is currently
// empty, which is never an imported/custom shape and must not claim to be.
func poolLessClusterError(clusterName, verb string, fieldPresent bool) *clierr.Error {
	if fieldPresent {
		return clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"cluster %q currently has no machine pools; %s applies only to a cluster with at least one machine pool",
			clusterName, verb))
	}
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"cluster %q has no machine pools; %s applies only to a provisioned cluster with machine pools (imported and custom clusters carry none)",
		clusterName, verb))
}

// poolNotFoundError refuses a family verb naming a pool the cluster does not
// carry, pointing at inspect cluster's enriched pool listing as the recovery.
func poolNotFoundError(clusterName, poolName string) *clierr.Error {
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"cluster %q has no machine pool named %q", clusterName, poolName)).
		WithHint(fmt.Sprintf("rancher inspect cluster %s", clierr.ShellQuote(clusterName)))
}

// autoscaledPoolGuardError is cluster scale's CLI-only honesty guard: the
// server accepts a manual quantity edit on an autoscaled pool and applies
// it, but the autoscaler re-asserts its own count within its loop, so the
// edit is silently reverted minutes later. Naming pause-autoscaling as the
// sanctioned override keeps that override visible as state, not a flag
// nobody can audit later.
func autoscaledPoolGuardError(clusterName, poolName string) *clierr.Error {
	return clierr.New(clierr.CodeValidation, fmt.Sprintf(
		"pool %q on cluster %q is managed by the cluster autoscaler; the server accepts a manual quantity edit but the autoscaler reasserts its own count minutes later - pause autoscaling first to scale manually",
		poolName, clusterName)).
		WithHint(fmt.Sprintf("rancher cluster pause-autoscaling %s", clierr.ShellQuote(clusterName)))
}

// provisioningClusterSensitive reads the provisioningcluster kind row's
// Sensitive flag - every family mutation threads it into its Effect's
// Destructive parameter, the same way every other bespoke PlanUpdate/
// PlanCreate caller in the codebase threads its own kind row's Sensitive
// flag. A command cannot carry a lighter gate than the generic surface
// (`patch provisioning-cluster`) already demands for the same object.
func provisioningClusterSensitive() bool {
	row, ok := kinds.Lookup("provisioningcluster")
	if !ok {
		panic("provisioningcluster kind row not found in kinds.go - renamed or removed without updating internal/engine/provisioning")
	}
	return row.Sensitive
}

// wrapWithContextLine wraps exec to append contextLine as an indented detail
// line after the base "<kind>/<name> <verb>" success text - the shape
// clear-autoscaling and pause/resume-autoscaling each need beyond the plain
// mutation text PlanUpdate's own exec produces.
func wrapWithContextLine(exec runtime.ExecuteFunc, contextLine string) runtime.ExecuteFunc {
	return func(ctx context.Context) (runtime.Result, error) {
		res, err := exec(ctx)
		if err != nil {
			return res, err
		}
		tr := runtime.TextLines(strings.TrimSuffix(res.Text, "\n"), contextLine)
		res.Text, res.StyledText = tr.Text, tr.StyledText
		return res, nil
	}
}

// planClusterFamilyMutation is the fetch→resolve→mutate→diff→plan spine
// scale, set-autoscaling, and clear-autoscaling share: GET the cluster,
// resolve the named pool by name to its current index, hand mutate the
// SAME mutable pool map that will be written back (findMachinePool's index
// is only ever taken from a value clusterMachinePools already deep-copied,
// so this is the one place any of the three pool fields change), then
// diff/PlanUpdate with the kind's own Sensitive flag as the Destructive
// parameter. mutate returns extra Effect.Warnings to append, or an error to
// refuse pre-Plan (before any dry-run write).
func planClusterFamilyMutation(
	ctx context.Context,
	clients engine.ClusterClientFactory,
	in runtime.Input,
	verb string,
	mutate func(cluster *unstructured.Unstructured, pool map[string]any, clusterName, poolName string) ([]string, error),
) (safety.Effect, runtime.ExecuteFunc, error) {
	client, obj, ns, name, err := fetchClusterFamilyTarget(ctx, clients, in)
	if err != nil {
		return safety.Effect{}, nil, err
	}
	poolName := in.String("pool")

	pools, found := clusterMachinePools(obj)
	if !found || len(pools) == 0 {
		return safety.Effect{}, nil, poolLessClusterError(name, verb, found)
	}
	idx, ok := findMachinePool(pools, poolName)
	if !ok {
		return safety.Effect{}, nil, poolNotFoundError(name, poolName)
	}
	pool, ok := pools[idx].(map[string]any)
	if !ok {
		return safety.Effect{}, nil, clierr.New(clierr.CodeValidation, fmt.Sprintf(
			"cluster %q pool %q is not a valid machine pool object", name, poolName))
	}

	warnings, err := mutate(obj, pool, name, poolName)
	if err != nil {
		return safety.Effect{}, nil, err
	}

	after := obj.DeepCopy()
	if err := unstructured.SetNestedSlice(after.Object, pools, "spec", "rkeConfig", "machinePools"); err != nil {
		return safety.Effect{}, nil, err
	}
	after.SetResourceVersion(obj.GetResourceVersion())

	diff := engine.DiffObjects(obj.Object, after.Object)
	eff, exec, err := engine.PlanUpdate(ctx, client, provisioningClusterGVR, ns, after, engine.LocalCluster, provisioningClusterDisplayKind, "Cluster", diff, provisioningClusterSensitive())
	if err != nil {
		return eff, exec, err
	}
	eff.Warnings = append(eff.Warnings, warnings...)
	return eff, exec, nil
}
