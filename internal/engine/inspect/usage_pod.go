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
func (c *usageCmd) collectByPod(ctx context.Context, client engine.ClusterClient, clusterID string, sec *sections, in runtime.Input) (runtime.Result, error) {
	namespace := in.String(resources.FlagNamespace)
	nodeName := in.String("node")
	podKind, _ := kinds.Lookup("pod")

	p, scoped, err := runtime.TypedProjectOn(ctx, in, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	var projectID string
	var members map[string]bool
	if scoped {
		projectID = p.ID
		members, err = p.Namespaces(ctx)
		if err != nil {
			return runtime.Result{}, err
		}
		if namespace != "" && !members[namespace] {
			return runtime.Result{}, projects.NotMemberError(namespace, projectID,
				"pass a project namespace, or omit --namespace to cover every namespace of the project")
		}
		// A membership list the caller was authorized to read, returning zero
		// namespaces, is a verified-empty scope: no pod could belong to it, so
		// no pod or metrics read is issued and no collector can degrade.
		if len(members) == 0 {
			return usageResult(byPod, usageRows{}, nil, sec), nil
		}
	}

	l, err := client.List(ctx, podKind.GVR, namespace, "")
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
	for _, pod := range filterPodsByNode(l.Items, nodeName) {
		if members != nil && !members[pod.GetNamespace()] {
			continue
		}
		key := pod.GetNamespace() + "/" + pod.GetName()
		grouped[key] = []unstructured.Unstructured{pod}
	}

	// A project's membership check has already proved --namespace exists.
	var checkNamespace, checkNode string
	if projectID == "" && len(l.Items) == 0 {
		checkNamespace = namespace
	}
	if len(grouped) == 0 {
		checkNode = nodeName
	}
	warnings, err := checkEmptyScope(ctx, client, checkNamespace, checkNode)
	if err != nil {
		return runtime.Result{}, err
	}

	rows, err := aggregateUsage(ctx, client, sec, grouped, namespaces)
	if err != nil {
		return runtime.Result{}, err
	}
	return withCheckWarnings(usageResult(byPod, rows, nil, sec), warnings), nil
}
