package engine

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// ContainerNames returns the name of every container in pod, spec.containers
// then spec.initContainers, so callers that walk a pod's containers (log
// fetching) see the whole pod rather than only its long-running containers -
// an init container stuck in CrashLoopBackOff is the canonical case its logs
// must not be silently skipped. Shared by every caller that walks a pod's
// containers (inspect pod, logs, support-bundle) so container discovery
// cannot drift between them.
func ContainerNames(pod map[string]any) []string {
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

// SelectorFrom decodes a deployment's FULL spec.selector (matchLabels AND
// matchExpressions) into a rendered label selector string. A nil or empty
// selector is an error - never "", which as a List selector would return
// every pod in the namespace and misrepresent it as the deployment's own.
func SelectorFrom(dep *unstructured.Unstructured) (string, error) {
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
