package inspect

import (
	"cmp"
	"context"

	"github.com/rancher/rancher-cli/internal/engine"
	"github.com/rancher/rancher-cli/internal/engine/kinds"
	"github.com/rancher/rancher-cli/internal/engine/projects"
	"github.com/rancher/rancher-cli/internal/engine/resources"
	"github.com/rancher/rancher-cli/internal/engine/steve"
	"github.com/rancher/rancher-cli/internal/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// collectByProject reads the project->namespace map of the downstream
// cluster (a cluster-wide namespace List against the downstream client),
// groups pods by owning project, and calls aggregateUsage per project bucket.
// Pods in unowned namespaces land in a "<none>" row. With --project, only
// that project's bucket is returned, named from the project the pipeline
// resolved; without it, the cluster's projects as steve lists them for the
// caller name the buckets. --namespace narrows to that namespace's pods (a
// member check applies when --project is also set); --node narrows to pods
// on that node.
func (c *usageCmd) collectByProject(ctx context.Context, client engine.ClusterClient, clusterID string, sec *sections, in runtime.Input) (runtime.Result, error) {
	namespace := in.String(resources.FlagNamespace)
	nodeName := in.String("node")
	podKind, _ := kinds.Lookup("pod")
	nsKind, _ := kinds.Lookup("namespace")

	displayByID := map[string]string{}
	var filterID string
	p, scoped, err := runtime.TypedProjectOn(ctx, in, clusterID)
	if err != nil {
		return runtime.Result{}, err
	}
	if scoped {
		filterID = p.ID
		displayByID[p.ID] = cmp.Or(p.DisplayName, p.ID)
	} else {
		root, err := c.roots.Root()
		if err != nil {
			return runtime.Result{}, err
		}
		list, err := steve.Projects(ctx, steve.RootGetter(root), clusterID)
		if err != nil {
			return runtime.Result{}, err
		}
		for _, proj := range list {
			displayByID[proj.ID] = cmp.Or(proj.DisplayName, proj.ID)
		}
	}

	// Cluster-wide namespace List on the downstream client to read each namespace's project.
	nsList, err := client.List(ctx, nsKind.GVR, "", "")
	if err != nil {
		return runtime.Result{}, err
	}
	nsByProject := map[string]string{} // namespace name → project id
	for i := range nsList.Items {
		ns := nsList.Items[i].GetName()
		var pid string // empty string means unowned
		if nsCluster, nsProject, ok := projects.ProjectOf(&nsList.Items[i]); ok && nsCluster == clusterID {
			pid = nsProject
		}
		nsByProject[ns] = pid
	}

	// Without --project, a --namespace the cluster does not list is missing.
	// The namespace list read above answers it, so no extra request is made.
	if namespace != "" && filterID == "" {
		if _, listed := nsByProject[namespace]; !listed {
			return runtime.Result{}, kinds.NotFoundCurated(nsKind, namespace, clusterID)
		}
	}

	// The membership rule holds under this grouping too: --namespace with
	// --project must name a member namespace.
	if namespace != "" && filterID != "" && nsByProject[namespace] != filterID {
		return runtime.Result{}, projects.NotMemberError(namespace, filterID,
			"pass a project namespace, or omit --namespace to cover every namespace of the project")
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

	// Pod List - namespace-scoped when --namespace narrows to one namespace.
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

	// Ensure every known project has a row, even with zero pods - but only in
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

	var checkNode string
	if noPods(grouped) {
		checkNode = nodeName
	}
	warnings, err := checkEmptyScope(ctx, client, "", checkNode)
	if err != nil {
		return runtime.Result{}, err
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
	// JSON row keys stay project ids. "<none>" has no entry
	// and renders as-is.
	return withCheckWarnings(usageResult(byProject, rows, displayByID, sec), warnings), nil
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
