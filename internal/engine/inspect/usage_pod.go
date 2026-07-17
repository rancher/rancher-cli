package inspect

import (
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// collectByPod produces one usageRow per pod keyed by namespace/name.
// Requests/limits come from spec.containers; usage from pod metrics. An absent
// pod-metrics entry reports zero usage (not an error), matching how the
// namespace grouping treats pods without metrics. --namespace narrows the pod
// List; --node filters pods by spec.nodeName client-side; --project scopes to
// member namespaces.
func (c *usageCmd) collectByPod(ctx context.Context, client engine.ClusterClient, clusterID string, in runtime.Input) (runtime.Result, error) {
	namespace := in.String(resources.FlagNamespace)
	nodeName := in.String("node")
	projectFlag, err := resources.EffectiveProject(c.clients, in)
	if err != nil {
		return runtime.Result{}, err
	}
	podKind, _ := kinds.Lookup("pod")

	var projectID string
	var members map[string]bool
	if projectFlag != "" {
		projectID, err = projects.ResolveProject(ctx, c.clients, clusterID, projectFlag)
		if err != nil {
			return runtime.Result{}, err
		}
		members, err = projects.ProjectNamespaces(ctx, client, projectID)
		if err != nil {
			return runtime.Result{}, err
		}
		if namespace != "" && !members[namespace] {
			return runtime.Result{}, projects.NotMemberError(namespace, projectID,
				"pass a member namespace, or omit --namespace to scope the whole project")
		}
	}

	listNS := namespace
	l, err := client.List(ctx, podKind.GVR, listNS, "")
	if err != nil {
		return runtime.Result{}, err
	}

	var namespaces []string
	if namespace != "" {
		namespaces = []string{namespace}
	}

	// Build per-pod grouped map: each pod is its own "scope" keyed by ns/name.
	// aggregateUsage sums per scope; a single-pod scope degenerates correctly.
	grouped := map[string][]unstructured.Unstructured{}
	for _, pod := range l.Items {
		if nodeName != "" {
			nn, _, _ := unstructured.NestedString(pod.Object, "spec", "nodeName")
			if nn != nodeName {
				continue
			}
		}
		if members != nil && !members[pod.GetNamespace()] {
			continue
		}
		key := pod.GetNamespace() + "/" + pod.GetName()
		grouped[key] = []unstructured.Unstructured{pod}
	}

	inner, err := aggregateUsage(ctx, client, clusterID, grouped, namespaces)
	if err != nil {
		return runtime.Result{}, err
	}
	rows, _ := inner.Data.(usageRows)
	return runtime.Result{
		Kind:     "ResourceUsage",
		Data:     usageEnvelope{By: byPod, Data: rows},
		Warnings: inner.Warnings,
	}, nil
}
