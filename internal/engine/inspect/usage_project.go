package inspect

import (
	"context"
	"fmt"

	"github.com/rancher/rancher-cli/internal/clierr"
	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// collectByProject fetches the full project→namespace map for the downstream
// cluster (two extra Lists: a management-plane v3 projects List against the
// local cluster + a cluster-wide namespace List against the downstream client),
// groups pods by owning project, and calls aggregateUsage per project bucket.
// Pods in unowned namespaces land in a "<none>" row. When --project X is set,
// only X's bucket is returned; X resolves against the already-fetched project
// list (never projects.ResolveProject, which would repeat the same List).
// --namespace narrows to that namespace's pods (a member check applies when
// --project is also set); --node narrows to pods on that node.
func (c *usageCmd) collectByProject(ctx context.Context, client engine.ClusterClient, clusterID string, in runtime.Input) (runtime.Result, error) {
	namespace := in.String(resources.FlagNamespace)
	nodeName := in.String("node")
	projectFlag, err := resources.EffectiveProject(c.clients, in)
	if err != nil {
		return runtime.Result{}, err
	}
	podKind, _ := kinds.Lookup("pod")
	nsKind, _ := kinds.Lookup("namespace")
	projectKind, _ := kinds.Lookup("project")

	// Fetch all projects from the management-plane (local cluster, scoped to
	// this downstream cluster's id — same query ResolveProject uses).
	mgmtClient, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}
	projectList, err := mgmtClient.List(ctx, projectKind.GVR, clusterID, "")
	if err != nil {
		return runtime.Result{}, err
	}

	// Build ns→projectID and projectID→displayName maps.
	displayByID := map[string]string{}
	for i := range projectList.Items {
		id := projectList.Items[i].GetName()
		display, _, _ := unstructured.NestedString(projectList.Items[i].Object, "spec", "displayName")
		if display == "" {
			display = id
		}
		displayByID[id] = display
	}

	// When --project X is given, resolve to a single id against the project
	// list already fetched above — calling projects.ResolveProject here would
	// repeat the identical management-plane List (and open a TOCTOU window
	// between the two fetches). Same match rule as ResolveProject: exact id,
	// else display name; ambiguous display names are an error.
	var filterID string
	if projectFlag != "" {
		var matches []string
		for id, display := range displayByID {
			if id == projectFlag || display == projectFlag {
				matches = append(matches, id)
			}
		}
		switch len(matches) {
		case 1:
			filterID = matches[0]
		case 0:
			return runtime.Result{}, clierr.New(clierr.CodeNotFound,
				fmt.Sprintf("project %q not found on cluster %s", projectFlag, clusterID))
		default:
			return runtime.Result{}, clierr.New(clierr.CodeAmbiguousSelection,
				fmt.Sprintf("project name %q matches multiple projects on cluster %s; pass the project id", projectFlag, clusterID))
		}
	}

	// Cluster-wide namespace List on the downstream client to read ownership labels.
	nsList, err := client.List(ctx, nsKind.GVR, "", "")
	if err != nil {
		return runtime.Result{}, err
	}
	nsByProject := map[string]string{} // namespace name → project id
	for i := range nsList.Items {
		ns := nsList.Items[i].GetName()
		pid, _, _ := unstructured.NestedString(nsList.Items[i].Object, "metadata", "labels", "field.cattle.io/projectId")
		nsByProject[ns] = pid // empty string means unowned
	}

	// UU-3's membership rule holds under this grouping too: --namespace with
	// --project must name a member namespace.
	if namespace != "" && filterID != "" && nsByProject[namespace] != filterID {
		return runtime.Result{}, projects.NotMemberError(namespace, filterID,
			"pass a member namespace, or omit --namespace to scope the whole project")
	}

	// Pod List — namespace-scoped when --namespace narrows to one namespace.
	pods, err := client.List(ctx, podKind.GVR, namespace, "")
	if err != nil {
		return runtime.Result{}, err
	}

	grouped := map[string][]unstructured.Unstructured{}
	for _, pod := range filterPodsByNode(pods.Items, nodeName) {
		pid := nsByProject[pod.GetNamespace()]
		if filterID != "" && pid != filterID {
			continue
		}
		bucket := pid
		if bucket == "" {
			bucket = "<none>"
		}
		grouped[bucket] = append(grouped[bucket], pod)
	}

	// Ensure every known project has a row, even with zero pods — but only in
	// the unfiltered case. --namespace/--node narrow a set, so a project with
	// no matching pods is an empty set (absent row), while --project names a
	// bucket, which always gets its row.
	switch {
	case filterID != "":
		if _, ok := grouped[filterID]; !ok {
			grouped[filterID] = nil
		}
	case namespace == "" && nodeName == "":
		for id := range displayByID {
			if _, ok := grouped[id]; !ok {
				grouped[id] = nil
			}
		}
	}

	// Scope the pod-metrics List to the one namespace when --namespace is set
	// (same rule fetchPodMetrics applies everywhere else).
	var namespaces []string
	if namespace != "" {
		namespaces = []string{namespace}
	}
	inner, err := aggregateUsage(ctx, client, clusterID, grouped, namespaces)
	if err != nil {
		return runtime.Result{}, err
	}
	rows, _ := inner.Data.(usageRows)
	return runtime.Result{
		Kind: "ResourceUsage",
		// names makes the table SCOPE column show project display names while
		// JSON row keys stay project ids (UU-18/UU-25). "<none>" has no entry
		// and renders as-is.
		Data:     usageEnvelope{By: byProject, Data: rows, names: displayByID},
		Warnings: inner.Warnings,
	}, nil
}
