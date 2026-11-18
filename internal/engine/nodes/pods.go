package nodes

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/runtime/safety"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The texts kubectl drain gives for a pod it refuses before it evicts
// anything.
const (
	localStorageText = "Pods with local storage (use --delete-emptydir-data to override)"
	unmanagedText    = "Pods that declare no controller (use --force to override)"
)

const (
	mirrorPodAnnotation = "kubernetes.io/config.mirror"
	daemonSetKind       = "DaemonSet"
	daemonSetAPIVersion = "apps/v1"
)

var podsGVR = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

// podPlan is what the preview says about the pods of a node.
type podPlan struct {
	// objects has one entry for every listed pod.
	objects []safety.Object
	// blockers are the distinct kubectl texts of the pods that block the
	// drain, in the order the pods are listed.
	blockers []string
	// daemonSet is true when a listed pod belongs to a DaemonSet.
	daemonSet bool
	// evictedNamespaces are the namespaces of the pods that will be evicted,
	// sorted, each once.
	evictedNamespaces []string
}

// listNodePods lists the pods of node in every namespace of the cluster id,
// as the caller. readErr is the error of a read the server refused or failed;
// err is a client that cannot list by field selector, an internal error.
func listNodePods(ctx context.Context, clients engine.ClusterClientFactory, id, node string) (pods []unstructured.Unstructured, readErr, err error) {
	client, err := clients.Cluster(ctx, id)
	if err != nil {
		return nil, err, nil
	}
	lister, ok := client.(engine.FieldSelectorLister)
	if !ok {
		return nil, nil, internalError("the cluster client cannot list by field selector")
	}
	list, err := lister.ListFieldSelector(ctx, podsGVR, "", "spec.nodeName="+node)
	if err != nil {
		return nil, err, nil
	}
	return list.Items, nil, nil
}

func internalError(msg string) *clierr.Error {
	return clierr.New(clierr.CodeGeneric, "internal error: "+msg)
}

// planPods applies kubectl drain's pod filters, with DaemonSet pods ignored,
// to pods.
func planPods(pods []unstructured.Unstructured, force, deleteEmptyDir bool) podPlan {
	sorted := slices.Clone(pods)
	slices.SortFunc(sorted, func(a, b unstructured.Unstructured) int {
		return strings.Compare(a.GetNamespace()+"/"+a.GetName(), b.GetNamespace()+"/"+b.GetName())
	})
	var plan podPlan
	for _, pod := range sorted {
		outcome, message := podOutcome(pod.Object, force, deleteEmptyDir)
		plan.objects = append(plan.objects, safety.Object{Kind: "pod", Name: pod.GetNamespace() + "/" + pod.GetName(), Outcome: outcome, Message: message})
		switch outcome {
		case safety.OutcomeBlocksTheDrain:
			if !slices.Contains(plan.blockers, message) {
				plan.blockers = append(plan.blockers, message)
			}
		case safety.OutcomeWillBeEvicted:
			if ns := pod.GetNamespace(); !slices.Contains(plan.evictedNamespaces, ns) {
				plan.evictedNamespaces = append(plan.evictedNamespaces, ns)
			}
		}
		if daemonSetPod(pod.Object) {
			plan.daemonSet = true
		}
	}
	slices.Sort(plan.evictedNamespaces)
	return plan
}

// podOutcome runs one pod through kubectl drain's filters in kubectl's
// order: a finished pod passes every filter except the mirror pod filter, a
// DaemonSet pod is skipped, a mirror pod is skipped, a pod with an emptyDir
// volume blocks the drain without --delete-emptydir-data, and a pod with no
// controller blocks it without --force. The filter for a pod that is being
// deleted passes, because Rancher never asks kubectl to skip those.
func podOutcome(pod map[string]any, force, deleteEmptyDir bool) (outcome, message string) {
	finished := podFinished(pod)
	_, _, controlled := controller(pod)
	if daemonSetPod(pod) && !finished {
		return safety.OutcomeSkipped, "DaemonSet pod"
	}
	if annotations, _, _ := unstructured.NestedStringMap(pod, "metadata", "annotations"); hasKey(annotations, mirrorPodAnnotation) {
		return safety.OutcomeSkipped, "mirror pod"
	}
	if !finished && hasEmptyDir(pod) && !deleteEmptyDir {
		return safety.OutcomeBlocksTheDrain, localStorageText
	}
	if !finished && !controlled && !force {
		return safety.OutcomeBlocksTheDrain, unmanagedText
	}
	return safety.OutcomeWillBeEvicted, ""
}

func hasKey(m map[string]string, key string) bool {
	_, ok := m[key]
	return ok
}

func podFinished(pod map[string]any) bool {
	phase, _, _ := unstructured.NestedString(pod, "status", "phase")
	return phase == "Succeeded" || phase == "Failed"
}

func hasEmptyDir(pod map[string]any) bool {
	volumes, _, _ := unstructured.NestedSlice(pod, "spec", "volumes")
	for _, v := range volumes {
		if volume, ok := v.(map[string]any); ok && volume["emptyDir"] != nil {
			return true
		}
	}
	return false
}

// controller is the kind and apiVersion of the owner that controls the pod;
// false when no owner does.
func controller(pod map[string]any) (kind, apiVersion string, ok bool) {
	owners, _, _ := unstructured.NestedSlice(pod, "metadata", "ownerReferences")
	for _, o := range owners {
		if owner, isMap := o.(map[string]any); isMap {
			if controls, _ := owner["controller"].(bool); controls {
				kind, _ = owner["kind"].(string)
				apiVersion, _ = owner["apiVersion"].(string)
				return kind, apiVersion, true
			}
		}
	}
	return "", "", false
}

// daemonSetPod reports whether a DaemonSet of the apps/v1 API controls the pod,
// as kubectl drain's DaemonSet filter reads it: a controller of kind DaemonSet
// in another API group, such as a custom DaemonSet resource, is an ordinary
// controller.
func daemonSetPod(pod map[string]any) bool {
	kind, apiVersion, _ := controller(pod)
	return kind == daemonSetKind && apiVersion == daemonSetAPIVersion
}

// podListWarning is the preview warning for a pod list that failed.
func podListWarning(node string, err error) string {
	ce := clierr.From(err)
	return fmt.Sprintf("the preview cannot list the pods on node %s (%s: %s); Rancher lists them as its system user, and kubectl drain's rules for pods still apply", node, ce.Code, ce.Message)
}
