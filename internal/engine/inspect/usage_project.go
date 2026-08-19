package inspect

import (
	"context"
	"errors"
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
func (c *usageCmd) collectByProject(ctx context.Context, client engine.ClusterClient, clusterID string, sec *sections, in runtime.Input) (runtime.Result, error) {
	namespace := in.String(resources.FlagNamespace)
	nodeName := in.String("node")
	projectFlag := resources.TypedProject(in)
	podKind, _ := kinds.Lookup("pod")
	nsKind, _ := kinds.Lookup("namespace")
	projectKind, _ := kinds.Lookup("project")

	// Fetch all projects from the management-plane (local cluster, scoped to
	// this downstream cluster's id — same query ResolveProject uses).
	mgmtClient, err := c.clients.Cluster(ctx, engine.LocalCluster)
	if err != nil {
		return runtime.Result{}, err
	}
	displayByID := map[string]string{}
	var filterID string
	memberFallback := false
	projectList, err := mgmtClient.List(ctx, projectKind.GVR, clusterID, "")
	if err != nil {
		// Member path (mirrors projects.ResolveProject): a project member
		// cannot LIST the collection, but a named --project can still be
		// fetched directly — their per-project usage stays reachable.
		var ce *clierr.Error
		if !errors.As(err, &ce) || ce.Code != clierr.CodeRBAC {
			return runtime.Result{}, err
		}
		if projectFlag == "" {
			return runtime.Result{}, ce.WithHint("you cannot list projects; pass " +
				"--project <project id> to scope usage to a project you have access to")
		}
		obj, gerr := projects.ResolveByGet(ctx, mgmtClient, clusterID, projectFlag, ce)
		if gerr != nil {
			return runtime.Result{}, gerr
		}
		display, _, _ := unstructured.NestedString(obj.Object, "spec", "displayName")
		if display == "" {
			display = projectFlag
		}
		displayByID[projectFlag] = display
		filterID = projectFlag
		memberFallback = true
	}

	// Build ns→projectID and projectID→displayName maps.
	if !memberFallback {
		for i := range projectList.Items {
			id := projectList.Items[i].GetName()
			display, _, _ := unstructured.NestedString(projectList.Items[i].Object, "spec", "displayName")
			if display == "" {
				display = id
			}
			displayByID[id] = display
		}
	}

	// When --project X is given, resolve to a single id against the project
	// list already fetched above — calling projects.ResolveProject here would
	// repeat the identical management-plane List (and open a TOCTOU window
	// between the two fetches). Uses the canonical MatchByIDOrDisplay algorithm
	// (union with ambiguity, same as ResolveProject and ResolveCluster).
	// The member fallback above already resolved filterID by direct get —
	// exact id only, nothing to match.
	if projectFlag != "" && !memberFallback {
		ids := make([]string, 0, len(displayByID))
		for id := range displayByID {
			ids = append(ids, id)
		}
		matches, _, _ := projects.MatchByIDOrDisplay(projectFlag, ids, func(id string) string { return displayByID[id] })
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

	// An ownership map the caller was authorized to read, showing the named
	// project owns no namespace, is a verified-empty scope: the bucket is a
	// verified zero, so no pod or metrics read is issued.
	if filterID != "" && !ownsAnyNamespace(nsByProject, filterID) {
		cpuReq, cpuLim, memReq, memLim := sumPodResources(nil)
		rows := usageRows{filterID: {
			CPURequest: cpuReq.String(), CPULimit: cpuLim.String(),
			MemoryRequest: memReq.String(), MemoryLimit: memLim.String(),
		}}
		return usageResult(byProject, rows, displayByID, sec), nil
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
	rows, err := aggregateUsage(ctx, client, sec, grouped, namespaces)
	if err != nil {
		return runtime.Result{}, err
	}
	// names makes the table SCOPE column show project display names while
	// JSON row keys stay project ids (UU-18/UU-25). "<none>" has no entry
	// and renders as-is.
	return usageResult(byProject, rows, displayByID, sec), nil
}

// ownsAnyNamespace reports whether the ownership map assigns at least one
// namespace to projectID.
func ownsAnyNamespace(nsByProject map[string]string, projectID string) bool {
	for _, pid := range nsByProject {
		if pid == projectID {
			return true
		}
	}
	return false
}
