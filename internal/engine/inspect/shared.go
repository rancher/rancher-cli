// Package inspect holds the read composites (`inspect pod`, `inspect
// deployment`, `top node`, `images`, `usage`) — bespoke ReadCommands that join
// several cluster reads into one document. Metrics and log fetch failures
// degrade to Result.Warnings; only core object fetch failures are command
// errors.
package inspect

import (
	"context"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// rejectProject enforces PC-3 for the composites that bypass Route
// (inspect pod/deployment, top node, images): the global --project flag
// would otherwise reach them and be silently ignored, exactly the class of
// no-op DR-004 removed --project for. Uses EffectiveProject (flag > context)
// so a context-set project rejects too — a flag-only check would let that
// same no-op back in through the context fallback. cmd is the command name
// for the message ("inspect pod", "top node", ...).
func rejectProject(clients engine.ClusterClientFactory, in runtime.Input, cmd string) error {
	project, err := resources.EffectiveProject(clients, in)
	if err != nil {
		return err
	}
	if project == "" {
		return nil
	}
	return clierr.New(clierr.CodeValidation, "--project does not apply to "+cmd).
		WithHint("drop --project for this command")
}

// podMetricsGVR is a metrics GVR constant — deliberately not a kind-table
// row: the metrics pseudo-kinds are not user-addressable nouns, only fetched
// internally here. nodeMetricsGVR (its sibling) lives in topnode.go, the sole
// consumer.
var podMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}

// ownerChainMaxDepth caps the owner walk (pod -> ReplicaSet -> Deployment is
// depth 2; the extra headroom covers StatefulSet/DaemonSet/Job shapes without
// risking an unbounded walk on a cyclic or malformed ownerRef chain).
const ownerChainMaxDepth = 3

// recursableOwnerKinds are the controller kinds worth walking one level
// further up (RS -> Deployment, being the only multi-hop case today).
var recursableOwnerKinds = map[string]bool{
	"ReplicaSet":  true,
	"StatefulSet": true,
	"DaemonSet":   true,
	"Job":         true,
}

// ownerChain walks obj's controller ownerRef chain, fetching each recursable
// owner in turn. Unknown owner kinds (no kind-table row, or not one of the
// recursable controller kinds) are reported as-is and end the walk — never an
// error, per the spec's risk note on custom controllers. A fetch failure
// keeps the ref info and adds a warning instead of aborting the walk.
func ownerChain(ctx context.Context, client engine.ClusterClient, ns string, obj *unstructured.Unstructured) ([]map[string]any, []string) {
	var owners []map[string]any
	var warnings []string
	current := obj
	for depth := 0; depth < ownerChainMaxDepth; depth++ {
		ref, ok := controllerRef(current)
		if !ok {
			break
		}
		owners = append(owners, map[string]any{"kind": ref.kind, "name": ref.name})
		if !recursableOwnerKinds[ref.kind] {
			break
		}
		k, found := kinds.LookupGVK(ref.apiVersion, ref.kind)
		if !found {
			break
		}
		next, err := client.Get(ctx, k.GVR, ns, ref.name)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("owner %s/%s unavailable: %v", ref.kind, ref.name, err))
			break
		}
		current = next
	}
	return owners, warnings
}

type ownerRef struct{ apiVersion, kind, name string }

// controllerRef returns the controller ownerRef (controller: true), falling
// back to the first ownerRef when none is marked controller.
func controllerRef(obj *unstructured.Unstructured) (ownerRef, bool) {
	refs, _, _ := unstructured.NestedSlice(obj.Object, "metadata", "ownerReferences")
	if len(refs) == 0 {
		return ownerRef{}, false
	}
	first, fOK := asOwnerRef(refs[0])
	for _, r := range refs {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if isController, _, _ := unstructured.NestedBool(m, "controller"); isController {
			return asOwnerRef(r)
		}
	}
	return first, fOK
}

func asOwnerRef(r any) (ownerRef, bool) {
	m, ok := r.(map[string]any)
	if !ok {
		return ownerRef{}, false
	}
	kind, _ := m["kind"].(string)
	name, _ := m["name"].(string)
	apiVersion, _ := m["apiVersion"].(string)
	if kind == "" || name == "" {
		return ownerRef{}, false
	}
	return ownerRef{apiVersion: apiVersion, kind: kind, name: name}, true
}

// metricsUnavailableWarning is the one wording for a degraded metrics.k8s.io
// fetch, shared by every composite that joins in node/pod metrics.
func metricsUnavailableWarning(clusterID string) string {
	return fmt.Sprintf("metrics.k8s.io unavailable on cluster %s", clusterID)
}

// containerNames returns the name of every container in pod, spec.containers
// then spec.initContainers, so callers that walk a pod's containers (log
// fetching) see the whole pod rather than only its long-running containers —
// an init container stuck in CrashLoopBackOff is the canonical case its logs
// must not be silently skipped.
func containerNames(pod map[string]any) []string {
	var names []string
	for _, field := range []string{"containers", "initContainers"} {
		items, _, _ := unstructured.NestedSlice(pod, "spec", field)
		for _, item := range items {
			cm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if name, _ := cm["name"].(string); name != "" {
				names = append(names, name)
			}
		}
	}
	return names
}

// selectorFrom decodes a deployment's FULL spec.selector (matchLabels AND
// matchExpressions) into a rendered label selector string. A nil or empty
// selector is an error — never "", which as a List selector would return
// every pod in the namespace and misrepresent it as the deployment's own.
func selectorFrom(dep *unstructured.Unstructured) (string, error) {
	selMap, found, err := unstructured.NestedMap(dep.Object, "spec", "selector")
	if err != nil {
		return "", fmt.Errorf("reading spec.selector: %w", err)
	}
	if !found || len(selMap) == 0 {
		return "", fmt.Errorf("deployment %s has no spec.selector", dep.GetName())
	}
	var ls metav1.LabelSelector
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(selMap, &ls); err != nil {
		return "", fmt.Errorf("decoding spec.selector: %w", err)
	}
	sel, err := metav1.LabelSelectorAsSelector(&ls)
	if err != nil {
		return "", fmt.Errorf("invalid spec.selector: %w", err)
	}
	rendered := sel.String()
	if rendered == "" {
		return "", fmt.Errorf("deployment %s has an empty spec.selector", dep.GetName())
	}
	return rendered, nil
}
